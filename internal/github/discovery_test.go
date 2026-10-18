package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestedTeamMembershipUsesCompleteInheritedMemberEvidence(t *testing.T) {
	for _, name := range []string{"later_member", "nonmember", "denied", "wrong_team", "stalled"} {
		t.Run(name, func(t *testing.T) {
			pages := 0
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/graphql" {
					t.Error("unexpected team operation")
					w.WriteHeader(500)
					return
				}
				var input struct {
					Query     string
					Variables map[string]any
				}
				if json.NewDecoder(r.Body).Decode(&input) != nil {
					t.Fatal("invalid GraphQL input")
				}
				if !strings.Contains(input.Query, "membership:ALL") || input.Variables["id"] != "TEAM12" || input.Variables["login"] != "reviewer" {
					t.Error("membership request widened or lost inherited coverage")
				}
				pages++
				if name == "denied" {
					json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": "denied"}}})
					return
				}
				id := "TEAM12"
				if name == "wrong_team" {
					id = "OTHER_TEAM"
				}
				rows := []any{map[string]any{"databaseId": int64(8 + pages), "login": "someone-else"}}
				more := name == "stalled" || (name == "later_member" && pages == 1)
				if name == "later_member" && pages == 2 {
					rows = []any{map[string]any{"databaseId": 43, "login": "reviewer"}}
				}
				cursor := any(nil)
				if more {
					cursor = "next"
				}
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"id": id, "databaseId": 12, "members": map[string]any{"nodes": rows, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": cursor}}}}})
			}))
			defer peer.Close()
			client, err := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-team-fixture"})
			if err != nil {
				t.Fatal(err)
			}
			member, err := client.TeamMember(context.Background(), RequestedTeam{ID: 12, NodeID: "TEAM12", Slug: "reviewers"}, Identity{ID: 43, Login: "reviewer"})
			wantError := name == "denied" || name == "wrong_team" || name == "stalled"
			if (err != nil) != wantError || (!wantError && member != (name == "later_member")) {
				t.Fatalf("membership=%v error=%v", member, err)
			}
			if name == "later_member" && pages != 2 {
				t.Fatal("membership page was omitted")
			}
		})
	}
}

func TestConditionalDiscoveryNeverMasksFailureOrChangesPublicationReader(t *testing.T) {
	for _, name := range []string{"revalidation", "read_error", "uncached_304"} {
		t.Run(name, func(t *testing.T) {
			calls, conditionals := 0, 0
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("If-None-Match") != "" {
					conditionals++
				}
				if name == "uncached_304" {
					w.WriteHeader(304)
					return
				}
				if name == "read_error" && calls == 2 {
					w.WriteHeader(503)
					return
				}
				w.Header().Set("ETag", "\"identity-one\"")
				if r.Header.Get("If-None-Match") == "\"identity-one\"" {
					w.WriteHeader(304)
					return
				}
				json.NewEncoder(w).Encode(Identity{ID: 43, Login: "reviewer"})
			}))
			defer peer.Close()
			client, err := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-cache-fixture"})
			if err != nil {
				t.Fatal(err)
			}
			reader := client.DiscoveryReader()
			first, err := reader.Identity(context.Background())
			if name == "uncached_304" {
				if err == nil {
					t.Fatal("uncached 304 was interpreted as valid empty evidence")
				}
				return
			}
			if err != nil || first.ID != 43 {
				t.Fatalf("initial identity: %v", err)
			}
			second, err := reader.Identity(context.Background())
			if name == "read_error" {
				if err == nil {
					t.Fatal("cached identity concealed a fresh server failure")
				}
				return
			}
			if err != nil || second.ID != 43 || conditionals != 1 {
				t.Fatal("conditional read lost its validated representation")
			}
			if _, err := client.Identity(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls != 3 || conditionals != 1 {
				t.Fatal("discovery cache changed the original publication/evidence reader")
			}
		})
	}
}
