package source

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type AnchorRange struct {
	Path  string `json:"path"`
	Side  string `json:"side"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}
type AnchorSet []AnchorRange

func (anchors AnchorSet) Contains(path, side string, line int) bool {
	if line < 1 || (side != "LEFT" && side != "RIGHT") {
		return false
	}
	for _, anchor := range anchors {
		if anchor.Path == path && anchor.Side == side && line >= anchor.Start && line <= anchor.End {
			return true
		}
	}
	return false
}

var hunkPattern = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(?:.*)$`)

func patchPath(value, prefix string) (string, error) {
	// Git separates a filename header from its optional suffix with a literal
	// tab. Actual tabs in a filename are C-quoted, so split before unquoting
	// without trimming legitimate spaces from the pathname.
	value, _, _ = strings.Cut(value, "\t")
	if value == "/dev/null" {
		return "", nil
	}
	if strings.HasPrefix(value, "\"") {
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", errors.New("invalid quoted immutable diff path")
		}
		value = decoded
	}
	if !strings.HasPrefix(value, prefix) || len(value) == len(prefix) {
		return "", errors.New("immutable diff has no canonical file path")
	}
	return strings.TrimPrefix(value, prefix), nil
}

// ParseAnchors validates complete hunk counts and uses the current canonical
// file path for both sides, including renames. Lines outside a visible hunk
// cannot become an inline comment simply because they exist in source.
func ParseAnchors(diff string) (AnchorSet, error) {
	anchors := AnchorSet{}
	oldPath, newPath := "", ""
	oldLine, newLine, oldRemaining, newRemaining := 0, 0, 0, 0
	inHunk := false
	add := func(path, side string, line int) {
		anchors = append(anchors, AnchorRange{Path: path, Side: side, Start: line, End: line})
	}
	for _, line := range strings.Split(diff, "\n") {
		if inHunk && (oldRemaining > 0 || newRemaining > 0) {
			if strings.HasPrefix(line, "\\ No newline at end of file") {
				continue
			}
			if len(line) == 0 {
				return nil, errors.New("immutable comparison ended inside a diff hunk")
			}
			path := newPath
			if path == "" {
				path = oldPath
			}
			if path == "" {
				return nil, errors.New("diff hunk has no canonical path")
			}
			switch line[0] {
			case ' ':
				oldRemaining--
				newRemaining--
				add(path, "LEFT", oldLine)
				add(path, "RIGHT", newLine)
				oldLine++
				newLine++
			case '-':
				oldRemaining--
				add(path, "LEFT", oldLine)
				oldLine++
			case '+':
				newRemaining--
				add(path, "RIGHT", newLine)
				newLine++
			default:
				return nil, errors.New("invalid immutable diff hunk content")
			}
			if oldRemaining < 0 || newRemaining < 0 {
				return nil, errors.New("immutable diff hunk counts do not match its content")
			}
			continue
		}
		if strings.HasPrefix(line, "diff --git ") {
			oldPath = ""
			newPath = ""
			inHunk = false
			continue
		}
		var err error
		if strings.HasPrefix(line, "--- ") {
			oldPath, err = patchPath(strings.TrimPrefix(line, "--- "), "a/")
			if err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			newPath, err = patchPath(strings.TrimPrefix(line, "+++ "), "b/")
			if err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, "@@") {
			match := hunkPattern.FindStringSubmatch(line)
			if match == nil {
				return nil, errors.New("unsupported immutable diff hunk")
			}
			parse := func(value string, defaultValue int) (int, error) {
				if value == "" {
					return defaultValue, nil
				}
				return strconv.Atoi(value)
			}
			oldLine, err = parse(match[1], 0)
			if err != nil {
				return nil, errors.New("diff line exceeds supported range")
			}
			newLine, err = parse(match[3], 0)
			if err != nil {
				return nil, errors.New("diff line exceeds supported range")
			}
			oldRemaining, err = parse(match[2], 1)
			if err != nil {
				return nil, errors.New("diff hunk exceeds supported range")
			}
			newRemaining, err = parse(match[4], 1)
			if err != nil {
				return nil, errors.New("diff hunk exceeds supported range")
			}
			if (oldRemaining > 0 && oldLine < 1) || (newRemaining > 0 && newLine < 1) {
				return nil, errors.New("diff hunk has invalid source coordinates")
			}
			inHunk = true
		}
	}
	if oldRemaining != 0 || newRemaining != 0 {
		return nil, errors.New("immutable comparison contains an incomplete diff hunk")
	}
	sort.Slice(anchors, func(i, j int) bool {
		a, b := anchors[i], anchors[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Side != b.Side {
			return a.Side < b.Side
		}
		return a.Start < b.Start
	})
	merged := AnchorSet{}
	for _, anchor := range anchors {
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if last.Path == anchor.Path && last.Side == anchor.Side && anchor.Start <= last.End+1 {
				last.End = max(last.End, anchor.End)
				continue
			}
		}
		merged = append(merged, anchor)
	}
	return merged, nil
}
