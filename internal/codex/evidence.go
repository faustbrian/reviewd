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
	"unicode/utf8"
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
	Revision   string `json:"revision"`
	Path       string `json:"path"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
	ByteOffset int    `json:"byte_offset"`
}
type documentQuery struct {
	Key        string `json:"key"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
	ByteOffset int    `json:"byte_offset"`
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

func page(content string, offset, limit, byteOffset int) (string, error) {
	if offset < 0 || limit < 0 || limit > 500 || byteOffset < 0 {
		return "", errors.New("source page requires offset >= 0 and limit between 1 and 500")
	}
	if !utf8.ValidString(content) {
		return "", errors.New("granted evidence is not supported UTF-8 text")
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
	joined := strings.Join(lines[start:end], "\n")
	if byteOffset > 0 && (start == len(lines) || byteOffset > len(lines[start]) || (byteOffset < len(lines[start]) && !utf8.RuneStart(lines[start][byteOffset]))) {
		return "", errors.New("source byte cursor is outside the current UTF-8 line")
	}
	remaining := joined[byteOffset:]
	cut, budget := 0, 60<<10
	for _, char := range remaining {
		size := utf8.RuneLen(char)
		cost := size
		if char == '"' || char == '\\' || char == '\n' || char == '\r' || char == '\t' || char == '\b' || char == '\f' {
			cost = 2
		} else if char < 32 || char == '<' || char == '>' || char == '&' || char == '\u2028' || char == '\u2029' {
			cost = 6
		}
		if budget < cost {
			break
		}
		budget -= cost
		cut += size
	}
	nextOffset, nextByte := end, 0
	if cut < len(remaining) {
		consumed := joined[:byteOffset+cut]
		advance := strings.Count(consumed, "\n")
		nextOffset = start + advance
		nextByte = len(consumed)
		if last := strings.LastIndexByte(consumed, '\n'); last >= 0 {
			nextByte = len(consumed) - last - 1
		}
	}
	encoded, err := json.Marshal(map[string]any{"offset": start, "byte_offset": byteOffset, "next_offset": nextOffset, "next_byte_offset": nextByte, "total_lines": len(lines), "eof": nextOffset == len(lines), "content": remaining[:cut]})
	return string(encoded), err
}

func (b *EvidenceBroker) Handle(tool string, args json.RawMessage) (string, error) {
	switch tool {
	case "reviewd_read_source":
		var query sourceQuery
		if err := strictJSON(args, &query); err != nil {
			return "", err
		}
		if !sourcePath(query.Path) || !sourceRevision(query.Revision) {
			return "", errors.New("source path or revision not granted")
		}
		content, ok := b.sources[query.Revision][query.Path]
		if !ok {
			return "", errors.New("source file not granted")
		}
		return page(content, query.Offset, query.Limit, query.ByteOffset)
	case "reviewd_list_source":
		var query struct {
			Revision string `json:"revision"`
			Offset   int    `json:"offset"`
			Limit    int    `json:"limit"`
		}
		if err := strictJSON(args, &query); err != nil {
			return "", err
		}
		if !sourceRevision(query.Revision) {
			return "", errors.New("source revision not granted")
		}
		if query.Offset < 0 || query.Limit < 0 || query.Limit > 500 {
			return "", errors.New("source manifest requires a nonnegative offset and at most 500 paths")
		}
		if query.Limit == 0 {
			query.Limit = 200
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
		start := min(query.Offset, len(paths))
		end := min(start+query.Limit, len(paths))
		// Bound encoded size as well as path count. A pathological individual
		// path is an explicit access error, not a silently dropped source file.
		for end > start {
			encoded, _ := json.Marshal(paths[start:end])
			if len(encoded) <= 60<<10 {
				break
			}
			end--
		}
		if start < len(paths) && end == start {
			return "", errors.New("source path exceeds the supported manifest page size")
		}
		data, err := json.Marshal(map[string]any{"paths": paths[start:end], "offset": start, "next_offset": end, "total_paths": len(paths), "eof": end == len(paths)})
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
		return page(content, query.Offset, query.Limit, query.ByteOffset)
	default:
		return "", errors.New("capability is not a permitted read-only evidence tool")
	}
}

func EvidenceTools() []map[string]any {
	text := func() map[string]any { return map[string]any{"type": "string"} }
	number := func(maximum int) map[string]any {
		return map[string]any{"type": "integer", "minimum": 0, "maximum": maximum}
	}
	revision := map[string]any{"type": "string", "enum": []string{"base", "head", "target"}}
	tool := func(name, description string, properties map[string]any, required []string) map[string]any {
		return map[string]any{"type": "function", "name": name, "description": description, "inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}
	}
	return []map[string]any{
		tool("reviewd_list_source", "Page granted immutable source paths at the merge base, head or trusted target base. Follow next_offset until eof; total_paths accounts for every granted file.", map[string]any{"revision": revision, "offset": number(1 << 30), "limit": number(500)}, []string{"revision"}),
		tool("reviewd_read_source", "Read immutable source by relative path, zero-based line offset and maximum 500-line limit. Follow next_offset and next_byte_offset until eof, including large single lines. No shell or filesystem access.", map[string]any{"revision": revision, "path": text(), "offset": number(1 << 30), "byte_offset": number(1 << 30), "limit": number(500)}, []string{"revision", "path"}),
		tool("reviewd_read_evidence", "Read a granted named requirement, diff, check or skill reference. Follow next_offset and next_byte_offset until eof; no network or credential access.", map[string]any{"key": text(), "offset": number(1 << 30), "byte_offset": number(1 << 30), "limit": number(500)}, []string{"key"}),
	}
}

func sourceRevision(revision string) bool {
	return revision == "base" || revision == "head" || revision == "target"
}
