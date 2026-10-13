package review

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

type CapturedSkill struct {
	Identity  SkillIdentity
	Path      string
	Documents map[string]string
	root      string
	closeOnce sync.Once
	closeErr  error
}

func (s *CapturedSkill) Close() error {
	s.closeOnce.Do(func() {
		if os.RemoveAll(s.root) != nil {
			s.closeErr = errors.New("could not remove owned frozen skill storage")
		}
	})
	return s.closeErr
}

// CaptureSkill freezes the selected installed package, not the operator's
// entire skill tree. References and helper source are granted as text only;
// capturing them is not authority to execute scripts or discover connectors.
func CaptureSkill(name, path, parent string) (capture *CapturedSkill, err error) {
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.New("selected installed review skill is unavailable")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, errors.New("selected skill path is invalid")
	}
	if filepath.Base(path) != "SKILL.md" {
		return nil, errors.New("reviewer.skill_path must point to the selected SKILL.md")
	}
	installed := filepath.Dir(path)
	root, err := os.MkdirTemp(parent, "reviewd-skill-")
	if err != nil {
		return nil, errors.New("could not create private frozen skill storage")
	}
	primaryDirectory := filepath.Base(installed)
	capture = &CapturedSkill{root: root, Path: filepath.Join(root, primaryDirectory, "SKILL.md"), Documents: map[string]string{}}
	owned := capture
	defer func() {
		if err != nil {
			err = errors.Join(err, owned.Close())
		}
	}()
	total := int64(0)
	type packageScope struct{ directory, label, destination string }
	packages := []packageScope{{installed, "skill", primaryDirectory}}
	seen := map[string]bool{filepath.Base(installed): true}
	dependencyLink := regexp.MustCompile(`\]\(\.\./([A-Za-z0-9_-]+)/SKILL\.md(?:#[^)]*)?\)`)
	for index := 0; index < len(packages); index++ {
		current := packages[index]
		if len(packages) > 16 {
			return nil, errors.New("selected skill has more than sixteen linked instruction packages; configure a bounded self-contained review skill")
		}
		err = filepath.WalkDir(current.directory, func(selected string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return errors.New("selected skill package could not be completely read")
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return errors.New("selected skill references must not be symlinks; keep reference files inside the selected installed package")
			}
			if entry.IsDir() {
				if selected != current.directory && (entry.Name() == ".git" || entry.Name() == "__pycache__" || entry.Name() == "node_modules" || entry.Name() == ".venv") {
					return filepath.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(selected))
			if ext != ".md" && ext != ".txt" && ext != ".py" && ext != ".json" && ext != ".toml" && ext != ".yaml" && ext != ".yml" {
				return nil
			}
			file, readErr := os.Open(selected)
			if readErr != nil {
				return errors.New("selected skill instruction/reference could not be opened")
			}
			info, statErr := file.Stat()
			if statErr != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
				file.Close()
				return errors.New("selected skill instructions/references must be regular files no larger than 1 MiB")
			}
			data, readErr := io.ReadAll(io.LimitReader(file, 1<<20+1))
			closeErr := file.Close()
			if readErr != nil || closeErr != nil || len(data) > 1<<20 || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
				return errors.New("selected skill instructions/reference text was incomplete or unsupported")
			}
			total += int64(len(data))
			if total > 8<<20 {
				return errors.New("selected skill instruction bundle exceeds 8 MiB")
			}
			relative, relErr := filepath.Rel(current.directory, selected)
			if relErr != nil {
				return errors.New("selected skill reference escaped its package")
			}
			if relative == "SKILL.md" && strings.TrimSpace(string(data)) == "" {
				return errors.New("selected skill contains no instructions")
			}
			capture.Documents[current.label+"/"+filepath.ToSlash(relative)] = string(data)
			destination := filepath.Join(root, current.destination, relative)
			if os.MkdirAll(filepath.Dir(destination), 0700) != nil || os.WriteFile(destination, data, 0600) != nil {
				return errors.New("selected skill could not be frozen privately")
			}
			if ext == ".md" {
				for _, link := range dependencyLink.FindAllStringSubmatch(string(data), -1) {
					packageName := link[1]
					if seen[packageName] {
						continue
					}
					candidate := filepath.Join(filepath.Dir(selected), "..", packageName, "SKILL.md")
					relativeDependency, relErr := filepath.Rel(current.directory, candidate)
					if relErr == nil && relativeDependency != ".." && !strings.HasPrefix(relativeDependency, ".."+string(filepath.Separator)) {
						continue
					}
					if filepath.Dir(filepath.Dir(candidate)) != filepath.Dir(current.directory) {
						return errors.New("selected skill instruction dependency escapes its declared sibling package boundary")
					}
					dependency, resolveErr := filepath.EvalSymlinks(candidate)
					if resolveErr != nil {
						return errors.New("selected skill has an unavailable explicitly linked instruction dependency; install the complete selected workflow")
					}
					seen[packageName] = true
					packages = append(packages, packageScope{filepath.Dir(dependency), "skill-dependency/" + packageName, packageName})
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if _, ok := capture.Documents["skill/SKILL.md"]; !ok {
		return nil, errors.New("selected skill contains no readable SKILL.md")
	}
	capture.Identity = SkillIdentity{Name: name, Digest: digest(capture.Documents)}
	return capture, nil
}
