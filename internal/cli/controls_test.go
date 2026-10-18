package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
)

func TestPersonalHoldsSurviveClientsAndPreservePendingWork(t *testing.T) {
	var member atomic.Bool
	invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", authorityPeer(t, func() map[string]any {
		return lifecyclePull(1, "open", true, false)
	}, func() []any { return []any{authorityEvent("INITIAL", "User", 43)} }, &member))
	initial := invoke("sync")
	if len(initial.Items) != 1 {
		t.Fatalf("expected one pending item: %+v", initial)
	}
	id := initial.Items[0].ID
	type report struct {
		lifecycleInbox
		Controls struct {
			PausedProjects []string `json:"paused_projects"`
			Snoozes        []struct {
				ItemID string `json:"item_id"`
			} `json:"snoozes"`
		} `json:"controls"`
	}
	run := func(args ...string) report {
		t.Helper()
		var out, diagnostics bytes.Buffer
		args = append([]string{"--config", path, "--json"}, args...)
		if code := Run(context.Background(), args, &out, &diagnostics, deps); code != 0 {
			t.Fatalf("personal control failed: %d %s", code, diagnostics.String())
		}
		var result report
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Items) != 1 || result.Items[0].ID != id || result.Items[0].State != initial.Items[0].State || len(result.Runs) != 0 {
			t.Fatalf("personal hold lost or started pending work: %+v", result)
		}
		return result
	}
	paused := run("pause", "owner/repo")
	if len(paused.Controls.PausedProjects) != 1 || paused.Controls.PausedProjects[0] != "github.com/owner/repo" {
		t.Fatalf("project pause was not canonical and visible: %+v", paused.Controls)
	}
	run("snooze", id, "--for", "2h")
	persisted := run("sync")
	if len(persisted.Controls.PausedProjects) != 1 || len(persisted.Controls.Snoozes) != 1 || persisted.Controls.Snoozes[0].ItemID != id {
		t.Fatalf("reopened client or discovery lost holds: %+v", persisted.Controls)
	}
	resumed := run("resume", "owner/repo")
	if len(resumed.Controls.PausedProjects) != 0 || len(resumed.Controls.Snoozes) != 1 {
		t.Fatalf("project resume changed independent item snooze: %+v", resumed.Controls)
	}
	woken := run("unsnooze", id)
	if len(woken.Controls.Snoozes) != 0 {
		t.Fatal("unsnooze did not release the item")
	}
}

func TestHeldExplicitStartCannotRunThroughHeadlessWatcher(t *testing.T) {
	for _, mode := range []string{"pause", "snooze"} {
		t.Run(mode, func(t *testing.T) {
			var member atomic.Bool
			invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", authorityPeer(t, func() map[string]any {
				return lifecyclePull(1, "open", true, false)
			}, func() []any { return []any{authorityEvent("INITIAL", "User", 43)} }, &member))
			initial := invoke("sync")
			id := initial.Items[0].ID
			invoke("start", id)
			if mode == "pause" {
				invoke("pause", "owner/repo")
			} else {
				invoke("snooze", id, "--until-head")
			}
			var output, diagnostics bytes.Buffer
			code := Run(context.Background(), []string{"--config", path, "watch", "--headless", "--once", "--json"}, &output, &diagnostics, deps)
			if code != 0 {
				t.Fatalf("held watcher failed: %d %s", code, diagnostics.String())
			}
			var result lifecycleInbox
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Items) != 1 || result.Items[0].State != "queued" || len(result.Runs) != 0 {
				t.Fatal("held explicit start was lost or executed")
			}
		})
	}
}
