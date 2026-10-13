package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEvidencePagingCanFinishAWholeLargeSingleLine(t *testing.T) {
	content := strings.Repeat("é", 100000)
	broker := NewEvidenceBroker(map[string]map[string]string{"head": {"minified.txt": content}}, nil)
	offset, byteOffset := 0, 0
	var recovered strings.Builder
	for request := 0; request < 20; request++ {
		args, _ := json.Marshal(map[string]any{"revision": "head", "path": "minified.txt", "offset": offset, "byte_offset": byteOffset, "limit": 1})
		text, err := broker.Handle("reviewd_read_source", args)
		if err != nil {
			t.Fatal(err)
		}
		if len(text) > 70000 {
			t.Fatal("evidence tool can overflow the private protocol with one minified line")
		}
		var page struct {
			Content    string `json:"content"`
			Offset     int    `json:"next_offset"`
			ByteOffset int    `json:"next_byte_offset"`
			EOF        bool   `json:"eof"`
		}
		if json.Unmarshal([]byte(text), &page) != nil {
			t.Fatal("invalid evidence page")
		}
		recovered.WriteString(page.Content)
		if page.EOF {
			if recovered.String() != content {
				t.Error("whole-line paging silently truncated or corrupted UTF-8 source")
			}
			return
		}
		if page.Offset == offset && page.ByteOffset <= byteOffset {
			t.Fatal("large-line paging stalled")
		}
		offset = page.Offset
		byteOffset = page.ByteOffset
	}
	t.Fatal("complete large source was not reachable")
}

func TestSourceManifestPagesDoNotHideLaterGrantedFiles(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 501; i++ {
		files[fmt.Sprintf("file-%03d.go", i)] = "source"
	}
	broker := NewEvidenceBroker(map[string]map[string]string{"head": files}, nil)
	text, err := broker.Handle("reviewd_list_source", json.RawMessage(`{"revision":"head","offset":400,"limit":100}`))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Paths []string `json:"paths"`
		Next  int      `json:"next_offset"`
		Total int      `json:"total_paths"`
		EOF   bool     `json:"eof"`
	}
	if json.Unmarshal([]byte(text), &result) != nil || len(result.Paths) != 100 || result.Next != 500 || result.Total != 501 || result.EOF {
		t.Fatal("source manifest has no complete pagination boundary")
	}
}
