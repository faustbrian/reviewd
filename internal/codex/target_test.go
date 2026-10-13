package codex

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBrokerKeepsTrustedTargetSeparateFromComparisonBaseAndHead(t *testing.T) {
	broker := NewEvidenceBroker(map[string]map[string]string{"target": {".reviewd": "trusted target"}, "base": {".reviewd": "older merge base"}, "head": {".reviewd": "untrusted proposed authority"}}, nil)
	for revision, want := range map[string]string{"target": "trusted target", "base": "older merge base", "head": "untrusted proposed authority"} {
		args, _ := json.Marshal(map[string]string{"revision": revision, "path": ".reviewd"})
		page, err := broker.Handle("reviewd_read_source", args)
		if err != nil || !strings.Contains(page, want) {
			t.Fatalf("distinct trusted/comparison revision lost: %s %s %v", revision, page, err)
		}
	}
}
