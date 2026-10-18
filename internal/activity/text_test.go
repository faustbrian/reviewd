package activity

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLatestPreviewBoundsAndRedactsBeforeTruncating(t *testing.T) {
	for _, size := range []int{2047, 2048, 2049} {
		got := Clean(strings.Repeat("a", size))
		if len(got) > 2048 || (size <= 2048 && len(got) != size) {
			t.Fatalf("unexpected byte budget: input=%d output=%d", size, len(got))
		}
	}
	canary := "ghp_" + strings.Repeat("A", 36)
	got := Clean(strings.Repeat("a", 2038) + canary + "\x1b[31m\n")
	if strings.Contains(got, "ghp_") || strings.Contains(got, "\x1b") || len(got) > 2048 {
		t.Fatal("secret/control survived a boundary-crossing preview")
	}
	if !utf8.ValidString(Clean(strings.Repeat("界", 1000))) {
		t.Fatal("truncation split a UTF-8 character")
	}
}
