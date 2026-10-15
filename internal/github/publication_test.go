package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// Live GitHub review-scoped lists expose diff positions but omit line/side.
// Canonical comment lookup must supply anchors without guessing from position.
func TestReviewCommentsResolveCanonicalAnchors(t *testing.T) {
	for _, scenario := range []string{"right", "outdated_left", "unavailable", "wrong_id", "wrong_review", "reply", "no_anchor", "already_anchored", "conflicting_summary"} {
		t.Run(scenario, func(t *testing.T) {
			head := strings.Repeat("a", 40)
			canonical := map[string]any{
				"id": 72, "pull_request_review_id": 71,
				"user": map[string]any{"id": 42, "login": "reviewer"},
				"path": "file.go", "body": "finding", "side": "RIGHT", "line": 6, "original_line": 6,
				"commit_id": head, "original_commit_id": head,
			}
			switch scenario {
			case "outdated_left":
				canonical["side"] = "LEFT"
				canonical["line"] = nil
			case "wrong_id":
				canonical["id"] = 73
			case "wrong_review":
				canonical["pull_request_review_id"] = 73
			case "reply":
				canonical["in_reply_to_id"] = 70
			case "no_anchor":
				delete(canonical, "line")
				delete(canonical, "original_line")
				delete(canonical, "side")
			}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("readback attempted %s", r.Method)
					http.Error(w, "read only", 405)
					return
				}
				switch r.URL.Path {
				case "/repos/owner/repo/pulls/17/reviews/71/comments":
					if scenario == "already_anchored" {
						json.NewEncoder(w).Encode([]any{canonical})
						return
					}
					if scenario == "conflicting_summary" {
						json.NewEncoder(w).Encode([]any{map[string]any{"id": 72, "pull_request_review_id": 71, "path": "wrong.go", "body": "stale summary", "side": "LEFT", "line": 19, "original_line": 19}})
						return
					}
					json.NewEncoder(w).Encode([]any{map[string]any{"id": 72, "pull_request_review_id": 71, "path": "file.go", "body": "finding", "position": 19}})
				case "/repos/owner/repo/pulls/comments/72":
					if scenario == "unavailable" {
						http.Error(w, "unavailable", 503)
						return
					}
					json.NewEncoder(w).Encode(canonical)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer peer.Close()
			client, err := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := client.ReviewComments(context.Background(), Pull{PullReference: PullReference{Repository: "owner/repo", Number: 17}, ID: 700, RepositoryID: 99}, 71)
			if scenario == "unavailable" || scenario == "wrong_id" || scenario == "wrong_review" {
				if err == nil {
					t.Fatalf("unverified canonical lookup accepted: %+v", rows)
				}
				return
			}
			if err != nil || len(rows) != 1 {
				t.Fatalf("readback: %+v %v", rows, err)
			}
			row := rows[0]
			if scenario == "no_anchor" {
				if row.Line != nil || row.OriginalLine != nil || row.Side != "" {
					t.Fatalf("legacy position invented an anchor: %+v", row)
				}
				return
			}
			side := "RIGHT"
			if scenario == "outdated_left" {
				side = "LEFT"
			}
			if row.ID != 72 || row.ReviewID != 71 || row.Side != side || row.OriginalLine == nil || *row.OriginalLine != 6 || row.OriginalCommitID != head {
				t.Fatalf("canonical anchor unavailable: %+v", row)
			}
			if scenario == "reply" && row.ReplyTo != 70 {
				t.Fatal("reply identity was lost")
			}
		})
	}
}

// Q1: pagination/order cannot join canonical comments by their row position.
func TestReviewCommentsCanonicalizeEveryPagedIdentity(t *testing.T) {
	head := strings.Repeat("b", 40)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected mutation %s", r.Method)
		}
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/17/reviews/71/comments":
			id := 73
			if r.URL.Query().Get("page") == "2" {
				id = 72
			} else {
				w.Header().Set("Link", `<https://api.github.com/unused?page=2>; rel="next"`)
			}
			json.NewEncoder(w).Encode([]any{map[string]any{"id": id, "pull_request_review_id": 71, "position": 99}})
		case "/repos/owner/repo/pulls/comments/72", "/repos/owner/repo/pulls/comments/73":
			id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/pulls/comments/"))
			line, side := 6, "RIGHT"
			if id == 73 {
				line, side = 9, "LEFT"
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "pull_request_review_id": 71, "original_commit_id": head, "original_line": line, "side": side})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()
	client, err := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := client.ReviewComments(context.Background(), Pull{PullReference: PullReference{Repository: "owner/repo", Number: 17}, ID: 700, RepositoryID: 99}, 71)
	if err != nil || len(rows) != 2 {
		t.Fatalf("complete inventory: %+v %v", rows, err)
	}
	for i, wanted := range []struct {
		id   int64
		line int
		side string
	}{{73, 9, "LEFT"}, {72, 6, "RIGHT"}} {
		row := rows[i]
		if row.ID != wanted.id || row.OriginalLine == nil || *row.OriginalLine != wanted.line || row.Side != wanted.side || row.OriginalCommitID != head {
			t.Fatalf("identity/anchor joined incorrectly: %+v", row)
		}
	}
}
