package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestLegacyPreparedReviewsKeepFollowingAfterUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name, state       string
		version           int
		unfollow, corrupt bool
	}{
		{name: "schema1_prepared", state: "prepared", version: 1},
		{name: "schema2_published", state: "published", version: 2},
		{name: "explicit_unfollow", state: "published", version: 2, unfollow: true},
		{name: "corrupt_body", state: "prepared", version: 2, corrupt: true},
		{name: "unprepared_json", state: "prepared", version: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase := 0
			_, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pull := lifecyclePull(1, "open", phase == 0, false)
				if phase >= 2 {
					pull["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
				}
				var value any = []any{}
				switch r.URL.Path {
				case "/user":
					value = map[string]any{"id": 43, "login": "reviewer"}
				case "/repos/owner/repo":
					value = map[string]any{"id": 99, "full_name": "owner/repo"}
				case "/repos/owner/repo/pulls":
					value = []any{pull}
				case "/graphql":
					value = lifecycleGraphQL(t, r, func(int) []any { return []any{} })
				}
				if err := json.NewEncoder(w).Encode(value); err != nil {
					t.Error(err)
				}
			}))
			invoke := func(command string, extra ...string) lifecycleInbox {
				t.Helper()
				var out, diagnostics bytes.Buffer
				args := append([]string{"--config", path, command}, extra...)
				args = append(args, "--json")
				if code := Run(context.Background(), args, &out, &diagnostics, deps); code != 0 {
					t.Fatalf("%s: %d %s", command, code, diagnostics.String())
				}
				var result lifecycleInbox
				if err := json.Unmarshal(out.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := config.Parse(raw, path)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := config.ResolveDefaults(doc, nil)
			if err != nil {
				t.Fatal(err)
			}
			directory, err := stateDirectory(policy, path)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "reviewd.sqlite"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", filepath.Join(directory, "reviewd.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			// Start with the actual old schema, not a downgraded current database.
			schema, err := os.ReadFile("../store/testdata/schema9.sql")
			if err != nil {
				t.Fatal(err)
			}
			lines := []string{}
			for _, line := range strings.Split(string(schema), "\n") {
				if !strings.HasPrefix(line, "INSERT INTO ") {
					lines = append(lines, line)
				}
			}
			if _, err = db.Exec(strings.Join(lines, "\n")); err != nil {
				t.Fatal(err)
			}
			prepared := []byte(`{}`)
			if tc.version != 0 {
				prepared, err = os.ReadFile(fmt.Sprintf("testdata/legacy-prepared-v%d.json", tc.version))
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.corrupt {
				prepared = []byte(strings.Replace(string(prepared), "**No findings.**", "tampered body", 1))
			}
			run := store.Run{ID: strings.Repeat("7", 32), Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "historical-review", State: store.State(tc.state), Configuration: json.RawMessage(`{}`)}
			record, _ := json.Marshal(run)
			if _, err = db.Exec(`INSERT INTO runs(id,repository,repository_id,pull_id,actor_id,work_key,state,record,updated_at,prepared,payload_id,thread_id,turn_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.Repository, 99, 701, 43, run.WorkKey, tc.state, string(record), "2026-10-17T00:00:00Z", string(prepared), "owned-payload", "owned-thread", "owned-turn"); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			initial := invoke("sync")
			if len(initial.Items) != 1 {
				t.Fatalf("initial request missing: %+v", initial)
			}
			if tc.unfollow {
				invoke("unfollow", initial.Items[0].ID)
			}
			phase = 1
			invoke("sync")
			phase = 2
			result := invoke("sync")
			want := tc.version != 0 && !tc.unfollow && !tc.corrupt
			found := false
			for _, item := range result.Items {
				if item.Number == 1 && item.Category == "reviews_owed" {
					found = true
					if item.Trigger != "head_updated" || item.State != "queued" {
						t.Fatalf("historical follow produced wrong work: %+v", item)
					}
				}
			}
			if found != want {
				t.Fatalf("legacy follow after upgrade/request removal/head update: got=%t want=%t items=%+v", found, want, result.Items)
			}
			durable, err := store.Open(context.Background(), directory)
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			old, err := durable.Get(context.Background(), run.ID)
			if err != nil || old.Executor != "" || old.State != run.State || strings.TrimSpace(string(old.Prepared)) != strings.TrimSpace(string(prepared)) {
				t.Fatalf("monitoring rewrote/replayed old review: %+v %v", old, err)
			}
		})
	}
}
