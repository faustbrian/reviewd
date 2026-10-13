package codex

import (
	"encoding/json"
	"strings"
	"testing"
)

// QA-13/26/X03: source access is a sealed capability, not arbitrary shell,
// network, file, or publisher access. Caller mutation must not change a run.
func TestEvidenceBrokerOnlyReadsFrozenGrantedEvidence(t *testing.T) {
	sources := map[string]map[string]string{"head": {"main.go": "first\nsecond\nthird\n"}, "base": {"main.go": "old\n"}}
	broker := NewEvidenceBroker(sources, map[string]string{"requirements": "trusted requirements"})
	sources["head"]["main.go"] = "changed after freeze"
	result, err := broker.Handle("reviewd_read_source", json.RawMessage(`{"revision":"head","path":"main.go","offset":1,"limit":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "second") || strings.Contains(result, "changed after freeze") || strings.Contains(result, "third") {
		t.Fatalf("invalid frozen source page: %q", result)
	}
	for _, input := range []struct{ tool, args string }{
		{"reviewd_read_source", `{"revision":"head","path":"../control/auth.json"}`},
		{"reviewd_read_source", `{"revision":"head","path":"/Users/operator/.config/gh/hosts.yml"}`},
		{"reviewd_read_source", `{"revision":"head","path":"main.go","command":"gh api -X POST"}`},
		{"reviewd_read_source", `{"revision":"future","path":"main.go"}`},
		{"reviewd_read_source", `{"revision":"head","path":"missing"}`},
		{"reviewd_read_source", `{"revision":"head","path":"main.go","offset":-1}`},
		{"reviewd_read_source", `{"revision":"head","path":"main.go","limit":501}`},
		{"reviewd_read_evidence", `{"key":"publisher_token"}`},
		{"exec_command", `{"cmd":"gh api -X POST"}`},
		{"reviewd_publish", `{}`},
	} {
		if _, err := broker.Handle(input.tool, json.RawMessage(input.args)); err == nil {
			t.Fatalf("capability escape allowed: %s %s", input.tool, input.args)
		}
	}
}

func TestEvidenceBrokerListsGrantedPathsAndHandlesEndOfFile(t *testing.T) {
	broker := NewEvidenceBroker(map[string]map[string]string{"head": {"z.go": "line", "a.go": ""}}, nil)
	paths, err := broker.Handle("reviewd_list_source", json.RawMessage(`{"revision":"head"}`))
	if err != nil || paths != `["a.go","z.go"]` {
		t.Fatalf("invalid source manifest: %q %v", paths, err)
	}
	result, err := broker.Handle("reviewd_read_source", json.RawMessage(`{"revision":"head","path":"z.go","offset":10}`))
	if err != nil || !strings.Contains(result, `"eof":true`) {
		t.Fatalf("end-of-file not explicit: %q %v", result, err)
	}
}
