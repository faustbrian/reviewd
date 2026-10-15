package review

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestDiscoveredStartRechecksCurrentAuthorPolicyBeforeSourceOrWorker(t *testing.T) {
	ctx := context.Background()
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			json.NewEncoder(w).Encode(map[string]any{"id": 43, "login": "reviewer"})
		case "/repos/owner/repo":
			json.NewEncoder(w).Encode(map[string]any{"id": 99, "full_name": "owner/repo"})
		case "/repos/owner/repo/pulls/1":
			json.NewEncoder(w).Encode(map[string]any{"id": 701, "number": 1, "state": "open", "draft": false, "title": "Owned PR", "body": "Required behavior", "updated_at": "2026-10-13T20:00:00Z", "user": map[string]any{"id": 43, "login": "renamed"}, "base": map[string]any{"sha": base, "ref": "main", "repo": map[string]any{"id": 99, "full_name": "owner/repo"}}, "head": map[string]any{"sha": head, "ref": "feature"}, "changed_files": 1, "commits": 1})
		default:
			t.Errorf("excluded author reached acquisition: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer peer.Close()
	client, err := github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-author-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	service := Service{GitHub: client, Store: durable, SkillPath: func(config.Snapshot) (string, error) { t.Fatal("excluded start captured skill"); return "", nil }, Worker: func(context.Context, WorkerRequest) (codex.ReviewOutput, error) {
		t.Fatal("excluded start ran worker")
		return codex.ReviewOutput{}, nil
	}}
	run, err := service.StartItem(ctx, nil, store.InboxItem{Repository: "github.com/owner/repo", Number: 1, PullID: 701, ActorID: 43, HeadSHA: head, TargetBaseSHA: base, Category: "reviews_owed"}, nil, false, false)
	if err == nil || !strings.Contains(err.Error(), "excludes this PR author") || run.ID != "" {
		t.Fatalf("stale queued item bypassed policy: %+v %v", run, err)
	}
}

func TestAutomaticPublisherRechecksAuthorBeforeAnyRemoteWrite(t *testing.T) {
	ctx := context.Background()
	durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	policy, err := config.Resolve(nil, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pull := github.Pull{PullReference: github.PullReference{Repository: policy.Repository, Number: 1}, RepositoryID: 99, ID: 701, Author: github.Identity{ID: 43, Login: "reviewer"}, HeadSHA: strings.Repeat("a", 40), TargetBaseSHA: strings.Repeat("b", 40)}
	prepared, err := Prepare(Boundary{Configuration: policy, Pull: pull, Actor: github.Identity{ID: 43}, Checks: github.CheckReport{Ready: true}}, Report{Conclusion: "APPROVE", Findings: []Finding{}, Gaps: []ResultGap{}})
	if err != nil {
		t.Fatal(err)
	}
	frozen, _ := json.Marshal(policy)
	run, _, err := durable.Create(ctx, store.Run{Repository: policy.Repository, RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "prior-allowed-self-review", State: store.Queued, Configuration: frozen})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := durable.Claim(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(prepared)
	if err := durable.Prepare(ctx, run.ID, prepared.PayloadID, payload, "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("excluded automatic publication contacted remote: %s", r.URL.Path)
		w.WriteHeader(500)
	}))
	defer peer.Close()
	client, err := github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-author-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.Parse([]byte("schema_version=1\n[actions]\npublish_review='auto'\n"), "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Publisher{GitHub: client, Store: durable}).Publish(ctx, doc, run.ID, prepared.PayloadID, true); err == nil || !strings.Contains(err.Error(), "excludes this PR author") {
		t.Fatalf("automatic publication bypassed exclusion: %v", err)
	}
	current, err := durable.Get(ctx, run.ID)
	if err != nil || current.State != store.Prepared {
		t.Fatal("denial consumed prepared result", current.State, err)
	}
}
