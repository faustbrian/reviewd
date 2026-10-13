package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// EvidenceBroker exposes only immutable source/evidence explicitly granted by
// the application. It cannot read the filesystem, execute code, or access a
// credential or network client.
type EvidenceBroker struct {
	sources   map[string]map[string]string
	documents map[string]string
}

func NewEvidenceBroker(sources map[string]map[string]string, documents map[string]string) *EvidenceBroker {
	b := &EvidenceBroker{sources: make(map[string]map[string]string), documents: make(map[string]string)}
	for revision, files := range sources {
		b.sources[revision] = make(map[string]string)
		for name, content := range files {
			b.sources[revision][name] = content
		}
	}
	for name, content := range documents {
		b.documents[name] = content
	}
	return b
}

type sourceQuery struct {
	Revision string `json:"revision"`
	Path     string `json:"path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}
type documentQuery struct {
	Key    string `json:"key"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func strictJSON(data json.RawMessage, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid evidence request: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("evidence request must contain one JSON object")
	}
	return nil
}

func sourcePath(name string) bool {
	return name != "" && name != "." && path.Clean(name) == name && !path.IsAbs(name) && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\\\x00:")
}

func page(content string, offset, limit int) (string, error) {
	if offset < 0 || limit < 0 || limit > 500 {
		return "", errors.New("source page requires offset >= 0 and limit between 1 and 500")
	}
	if limit == 0 {
		limit = 200
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") || content == "" {
		lines = lines[:len(lines)-1]
	}
	start := min(offset, len(lines))
	end := min(start+limit, len(lines))
	encoded, err := json.Marshal(map[string]any{"offset": start, "next_offset": end, "total_lines": len(lines), "eof": end == len(lines), "content": strings.Join(lines[start:end], "\n")})
	return string(encoded), err
}

func (b *EvidenceBroker) Handle(tool string, args json.RawMessage) (string, error) {
	switch tool {
	case "reviewd_read_source":
		var query sourceQuery
		if err := strictJSON(args, &query); err != nil {
			return "", err
		}
		if !sourcePath(query.Path) || (query.Revision != "head" && query.Revision != "base") {
			return "", errors.New("source path or revision not granted")
		}
		content, ok := b.sources[query.Revision][query.Path]
		if !ok {
			return "", errors.New("source file not granted")
		}
		return page(content, query.Offset, query.Limit)
	case "reviewd_list_source":
		var query struct {
			Revision string `json:"revision"`
		}
		if err := strictJSON(args, &query); err != nil {
			return "", err
		}
		if query.Revision != "head" && query.Revision != "base" {
			return "", errors.New("source revision not granted")
		}
		files, ok := b.sources[query.Revision]
		if !ok {
			return "", errors.New("source revision not granted")
		}
		paths := make([]string, 0, len(files))
		for name := range files {
			paths = append(paths, name)
		}
		sort.Strings(paths)
		data, err := json.Marshal(paths)
		return string(data), err
	case "reviewd_read_evidence":
		var query documentQuery
		if err := strictJSON(args, &query); err != nil {
			return "", err
		}
		content, ok := b.documents[query.Key]
		if !ok {
			return "", errors.New("evidence not granted")
		}
		return page(content, query.Offset, query.Limit)
	default:
		return "", errors.New("capability is not a permitted read-only evidence tool")
	}
}

func EvidenceTools() []map[string]any {
	text := func() map[string]any { return map[string]any{"type": "string"} }
	number := func(maximum int) map[string]any {
		return map[string]any{"type": "integer", "minimum": 0, "maximum": maximum}
	}
	revision := map[string]any{"type": "string", "enum": []string{"base", "head"}}
	tool := func(name, description string, properties map[string]any, required []string) map[string]any {
		return map[string]any{"type": "function", "name": name, "description": description, "inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}
	}
	return []map[string]any{
		tool("reviewd_list_source", "List every granted immutable source file at the pinned base or head.", map[string]any{"revision": revision}, []string{"revision"}),
		tool("reviewd_read_source", "Read immutable source by relative path, zero-based line offset and maximum 500-line limit; no shell or filesystem access.", map[string]any{"revision": revision, "path": text(), "offset": number(1 << 30), "limit": number(500)}, []string{"revision", "path"}),
		tool("reviewd_read_evidence", "Read a granted named requirement, diff, check or skill reference by zero-based line offset; no network or credential access.", map[string]any{"key": text(), "offset": number(1 << 30), "limit": number(500)}, []string{"key"}),
	}
}
