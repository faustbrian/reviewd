package dispatch

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

type numberedRemote struct{ remoteFixture }

func (r numberedRemote) Pull(ctx context.Context, repository string, number int) (github.Pull, error) {
	pull, err := r.remoteFixture.Pull(ctx, repository, number)
	pull.ID, pull.Number = int64(700+number), number
	pull.URL = fmt.Sprintf("https://%s/pull/%d", repository, number)
	return pull, err
}

func TestAcceptedRunExplicitStopAndDeadlineRemainRunScoped(t *testing.T) {
	for _, lane := range []string{"review", "feedback"} {
		for _, stop := range []string{"cancel", "deadline"} {
			t.Run(lane+"/"+stop, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
				if err != nil {
					t.Fatal(err)
				}
				defer durable.Close()
				entered := make(chan context.Context, 2)
				service := Service{Store: durable, GitHub: numberedRemote{}, LockDirectory: filepath.Join(t.TempDir(), "locks"), Launch: func(worker context.Context, request agent.Request) (agent.Output, error) {
					output := agent.Output{ThreadID: "session-" + request.URL[len(request.URL)-2:], Text: "Partial skill output."}
					if err := request.OnProgress(output); err != nil {
						return output, err
					}
					entered <- worker
					<-worker.Done()
					return output, worker.Err()
				}}
				type result struct {
					run store.Run
					err error
				}
				start := func(parent context.Context, number int) <-chan result {
					done := make(chan result, 1)
					go func() {
						run, err := service.Run(parent, nil, Job{Purpose: lane, Target: github.PullReference{Repository: "github.com/owner/repo", Number: number, URL: fmt.Sprintf("https://github.com/owner/repo/pull/%d", number)}})
						done <- result{run, err}
					}()
					return done
				}
				sentinelDone := start(ctx, 18)
				var selectedDone <-chan result
				selectedConsumed, sentinelConsumed := false, false
				defer func() {
					cancel()
					if selectedDone != nil && !selectedConsumed {
						<-selectedDone
					}
					if !sentinelConsumed {
						<-sentinelDone
					}
				}()
				var sentinel context.Context
				select {
				case sentinel = <-entered:
				case <-ctx.Done():
					t.Fatal("sentinel did not start")
				}
				selectedCtx := ctx
				if stop == "deadline" {
					var end context.CancelFunc
					selectedCtx, end = context.WithTimeout(ctx, 2*time.Second)
					defer end()
				}
				selectedDone = start(selectedCtx, 17)
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("selected run did not start")
				}
				runs, err := durable.List(ctx)
				if err != nil || len(runs) != 2 {
					t.Fatalf("accepted runs missing: %+v %v", runs, err)
				}
				if stop == "cancel" {
					for _, run := range runs {
						if run.Number == 17 {
							if _, err := durable.RequestCancellation(ctx, run.ID); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				var selected result
				select {
				case selected = <-selectedDone:
					selectedConsumed = true
				case <-ctx.Done():
					t.Fatal("selected run did not honor its stop")
				}
				wantState, wantError := store.Cancelled, context.Canceled
				if stop == "deadline" {
					wantState, wantError = store.AgentInterrupted, context.DeadlineExceeded
				}
				if !errors.Is(selected.err, wantError) || selected.run.State != wantState || selected.run.ThreadID != "session-17" || len(selected.run.Outcome) == 0 || sentinel.Err() != nil {
					t.Fatalf("wrong or unscoped stop: %+v error=%v sentinel=%v", selected.run, selected.err, sentinel.Err())
				}
				for _, run := range runs {
					if run.Number == 18 {
						current, err := durable.Get(ctx, run.ID)
						if err != nil || current.State != store.AgentRunning || current.CancellationRequested {
							t.Fatalf("unrelated accepted run was stopped: %+v %v", current, err)
						}
					}
				}
				cancel()
				<-sentinelDone
				sentinelConsumed = true
			})
		}
	}
}
