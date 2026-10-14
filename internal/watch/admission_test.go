package watch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

// Observe real admission through the privileged source boundary. The source
// adapter stops there: this contract concerns which eligible work starts, not
// immutable Git acquisition or model quality.
func TestAdmissionPrefersFreshRequestsWithoutStarvingOlderOptionalWork(t *testing.T) {
	f := newAdmissionFixture(t)
	defer func() { _ = f.durable.Close() }()
	f.add(t, 1, "new_pull_request", f.now)
	f.add(t, 2, "review_requested", f.now)
	if got := f.dispatch(t); got != 2 {
		t.Fatalf("fresh explicit request did not precede optional discovery: PR %d", got)
	}
	// New request traffic continues after each restart/refill. Reopen the real
	// profile each time so in-memory counters cannot manufacture fairness.
	for minute := 1; minute <= 20; minute++ {
		f.now = f.now.Add(time.Minute)
		f.add(t, 10+minute, "review_requested", f.now)
		if err := f.durable.Close(); err != nil {
			t.Fatal(err)
		}
		var err error
		f.durable, err = store.Open(context.Background(), f.directory)
		if err != nil {
			t.Fatal(err)
		}
		if f.dispatch(t) == 1 {
			return
		}
	}
	t.Fatal("continuous explicit requests starved the older eligible optional PR through repeated refills and restarts")
}

type admissionFixture struct {
	durable   *store.Store
	directory string
	manager   *config.Manager
	client    *github.Client
	now       time.Time
}

func newAdmissionFixture(t *testing.T) *admissionFixture {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "profile")
	durable, err := store.Open(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	manager, err := config.NewManager([]byte("schema_version=1\n[github]\nrepositories=[\"owner/repo\"]\n[limits]\nmax_concurrent_reviews=1\n"), filepath.Join(directory, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			fmt.Fprint(w, `{"id":43,"login":"reviewer"}`)
		case r.URL.Path == "/repos/owner/repo":
			fmt.Fprint(w, `{"id":99,"full_name":"owner/repo"}`)
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/pulls/"):
			number, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/pulls/"))
			if err != nil {
				t.Errorf("unexpected pull endpoint %s", r.URL.Path)
				w.WriteHeader(500)
				return
			}
			fmt.Fprintf(w, `{"id":%d,"number":%d,"state":"open","draft":false,"title":"Owned admission fixture","body":"","updated_at":"2026-10-14T00:00:00Z","user":{"id":42,"login":"author"},"base":{"sha":%q,"ref":"main","repo":{"id":99,"full_name":"owner/repo"}},"head":{"sha":%q,"ref":"feature"},"changed_files":1,"commits":1}`, 700+number, number, strings.Repeat("a", 40), strings.Repeat("b", 40))
		case r.URL.Path == "/repos/owner/repo/branches/main":
			fmt.Fprint(w, `{"protected":false}`)
		case r.URL.Path == "/repos/owner/repo/rules/branches/main":
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected admission request %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(peer.Close)
	client, err := github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-admission-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	return &admissionFixture{durable: durable, directory: directory, manager: manager, client: client, now: time.Now().UTC()}
}

func (f *admissionFixture) add(t *testing.T, number int, trigger string, arrived time.Time) {
	t.Helper()
	ctx := context.Background()
	before, err := f.durable.DiscoveryScope(ctx, "github.com/owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	after := before
	after.ActorID, after.RepositoryID, after.State = 43, 99, "ok"
	items, err := f.durable.DiscoveryItems(ctx, before.Repository, 43)
	if err != nil {
		t.Fatal(err)
	}
	items = append(items, store.InboxItem{Repository: before.Repository, ActorID: 43, PullID: int64(700 + number), Number: number, Category: "reviews_owed", Trigger: trigger, Action: "auto", State: "queued", EventKey: fmt.Sprintf("request-%d", number), HeadSHA: strings.Repeat("b", 40), TargetBaseSHA: strings.Repeat("a", 40)})
	if err := f.durable.ApplyDiscovery(ctx, before, after, []store.Observation{{PullID: int64(700 + number), Number: number, Record: json.RawMessage(`{}`)}}, items); err != nil {
		t.Fatal(err)
	}
	// This owned fixture advances arrival time without sleeping. Store's real
	// discovery API uses wall-clock time; only this newly arrived item's valid
	// timestamp is seeded, while prior durable ages/retry facts stay untouched.
	db, err := sql.Open("sqlite", filepath.Join(f.directory, "reviewd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE inbox_items SET record=json_set(record,'$.updated_at',?) WHERE repository=? AND pull_id=?`, arrived.Format(time.RFC3339Nano), before.Repository, 700+number); err != nil {
		t.Fatal(err)
	}
}

func (f *admissionFixture) dispatch(t *testing.T) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	selected := make(chan int, 1)
	stop := errors.New("owned source admission checkpoint")
	engine := &Engine{Store: f.durable, Configuration: f.manager, Now: func() time.Time { return f.now }, Reviewer: func(client *github.Client) review.Service {
		return review.Service{Store: f.durable, GitHub: client, SkillPath: func(config.Snapshot) (string, error) { return "", errors.New("fixture never starts a model") }, Worker: func(context.Context, review.WorkerRequest) (codex.ReviewOutput, error) {
			t.Error("admission fixture unexpectedly started a model")
			return codex.ReviewOutput{}, stop
		}, Source: func(_ context.Context, pull github.Pull) (*source.Snapshot, error) {
			selected <- pull.Number
			return nil, stop
		}}
	}}
	engine.initialize()
	engine.client, engine.admitted = f.client, map[string]bool{}
	defer engine.Close()
	if err := engine.Admit(ctx); err != nil || engine.Active() != 1 {
		t.Fatalf("no eligible admission: active=%d err=%v", engine.Active(), err)
	}
	if err := engine.Complete(ctx, true); !errors.Is(err, stop) {
		t.Fatalf("source boundary was not reached: %v", err)
	}
	select {
	case number := <-selected:
		return number
	default:
		t.Fatal("admission did not reach an actual source acquisition")
		return 0
	}
}
