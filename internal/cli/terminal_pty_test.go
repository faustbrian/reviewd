package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/store"
)

func terminalFixture(t *testing.T) (string, *store.Store, store.InboxItem) {
	t.Helper()
	ctx := context.Background()
	directory := t.TempDir()
	path, state := filepath.Join(directory, "config.toml"), filepath.Join(directory, "state")
	data := "schema_version=1\n[github]\nrepositories=[\"owner/repo\"]\n[state]\ndirectory=" + strconv.Quote(state) + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	scopes, err := s.ConfigureDiscovery(ctx, []string{"github.com/owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	previous := scopes["github.com/owner/repo"]
	next := previous
	next.ActorID, next.RepositoryID, next.State = 43, 99, "ok"
	item := store.InboxItem{Repository: next.Repository, PullID: 701, Number: 17, ActorID: 43, Category: "reviews_owed", Trigger: "review_requested", Action: "ask", State: "pending_decision", EventKey: "displayed-event", HeadSHA: strings.Repeat("a", 40), TargetBaseSHA: strings.Repeat("b", 40), ContextKey: "captured-context", Title: "配送 café review", URL: "https://other.invalid/untrusted", Explanation: "A human requested your review."}
	if err := s.ApplyDiscovery(ctx, previous, next, []store.Observation{{PullID: item.PullID, Number: item.Number, HeadSHA: item.HeadSHA, Record: json.RawMessage(`{}`)}}, []store.InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.Inbox(ctx)
	if err != nil || len(items) != 1 {
		t.Fatal("could not seed terminal item")
	}
	return path, s, items[0]
}

func TestTerminalEventDecisionCannotAuthorizeNewerWork(t *testing.T) {
	path, s, item := terminalFixture(t)
	ctx := context.Background()
	invoke := func(kind, event string) int {
		t.Helper()
		var output, diagnostics strings.Builder
		code := Run(ctx, []string{"--config", path, kind, item.ID, "--event", event, "--json"}, &output, &diagnostics, Dependencies{})
		if code != 0 && event == item.EventKey {
			t.Fatalf("displayed event could not be selected: %d %s", code, diagnostics.String())
		}
		return code
	}
	if invoke("start", item.EventKey) != 0 {
		t.Fatal("current event start failed")
	}
	scope, err := s.DiscoveryScope(ctx, item.Repository)
	if err != nil {
		t.Fatal(err)
	}
	next := item
	next.EventKey, next.HeadSHA, next.State = "newer-event", strings.Repeat("c", 40), "pending_decision"
	if err := s.ApplyDiscovery(ctx, scope, scope, []store.Observation{{PullID: item.PullID, Number: item.Number, HeadSHA: next.HeadSHA, Record: json.RawMessage(`{}`)}}, []store.InboxItem{next}); err != nil {
		t.Fatal(err)
	}
	if err := s.SnoozeItem(ctx, item.ID, time.Now().Add(time.Hour), false, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Both choices were opened from an older terminal frame.
	for _, kind := range []string{"start", "skip"} {
		var output, diagnostics strings.Builder
		if code := Run(ctx, []string{"--config", path, kind, item.ID, "--event", item.EventKey}, &output, &diagnostics, Dependencies{}); code == 0 {
			t.Fatalf("stale %s authorized the newly observed event", kind)
		}
	}
	items, _, err := s.Inbox(ctx)
	controls, controlErr := s.Controls(ctx, time.Now())
	if err != nil || controlErr != nil || len(items) != 1 || items[0].EventKey != next.EventKey || items[0].State != "pending_decision" || items[0].DecisionID != "" || len(controls.Snoozes) != 1 {
		t.Fatal("stale terminal choice mutated newer work or cleared its hold")
	}
}

func TestInteractiveInboxKeysResizeAndTerminalRestoration(t *testing.T) {
	path, s, item := terminalFixture(t)
	binary := filepath.Join(t.TempDir(), "reviewd")
	build := exec.Command("go", "build", "-o", binary, "./cmd/reviewd")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native CLI: %v %s", err, output)
	}
	// Python is also the repository's check runner prerequisite. The PTY owns
	// all child processes, exercises actual termios and never launches a browser.
	program := `
import errno,fcntl,json,os,re,select,signal,struct,subprocess,sys,termios,time
binary,config,mode=sys.argv[1:]
master,slave=os.openpty()
original=termios.tcgetattr(slave)
fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',24,92,0,0))
env=os.environ.copy(); env['TERM']='xterm-256color'
p=subprocess.Popen([binary,'--config',config,'inbox','--interactive'],stdin=slave,stdout=slave,stderr=slave,env=env,start_new_session=True)
output=bytearray()
def wait_for(text):
 deadline=time.monotonic()+8
 while text.encode() not in output:
  if time.monotonic()>deadline or p.poll() is not None:
   raise AssertionError('terminal inbox did not render '+repr(text)+': '+repr(bytes(output)[-1600:]))
  ready,_,_=select.select([master],[],[],.1)
  if ready:
   try: output.extend(os.read(master,65536))
   except OSError as exc:
    if exc.errno!=errno.EIO: raise
def send(data): os.write(master,data)
try:
 wait_for('Reviews owed')
 wait_for('My PR feedback')
 wait_for('Running')
 wait_for('Publication')
 wait_for('café')
 if mode.startswith('feedback'):
  send(b'h'); wait_for('Handle PR feedback'); wait_for('Fix/commit/push/GitHub')
 else:
  send(b's'); wait_for('Start private review')
 if mode in ('dismiss','feedback-dismiss'):
  send(b'\x1b'); time.sleep(.15); send(b'q')
 elif mode=='feedback':
  send(b'm'); wait_for('Feedback model (enter'); send(b'fixture-feedback-model\r')
  send(b'e'); wait_for('Reasoning effort'); send(b'low\r')
  send(b'y'); wait_for('Feedback permission queued'); send(b'q')
 elif mode=='start':
  send(b'm'); wait_for('Reviewer model'); send(b'fixture-review-model\r')
  send(b'e'); wait_for('Reasoning effort'); send(b'low\r')
  send(b'y'); wait_for('Private review queued')
  fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',10,28,0,0)); os.kill(p.pid,signal.SIGWINCH)
  time.sleep(.15); send(b'q')
 else:
  send(b'\x03')
 p.wait(timeout=8)
 if mode!='interrupt' and p.returncode!=0: raise AssertionError('unexpected terminal exit '+str(p.returncode))
 if mode=='interrupt' and p.returncode!=130: raise AssertionError('interrupt did not retain exit 130')
 restored=termios.tcgetattr(slave)
 # Darwin sets PENDIN itself when canonical mode is restored, even in a
 # kernel-only Python raw/cooked round trip. All operational settings and
 # control characters must match; do not confuse that transient flag with raw mode.
 original[3]&=~getattr(termios,'PENDIN',0); restored[3]&=~getattr(termios,'PENDIN',0)
 if restored!=original: raise AssertionError('terminal modes were not restored: '+repr([(i,a,b) for i,(a,b) in enumerate(zip(original,restored)) if a!=b]))
 if re.search(rb'\x1b\[\?(1000|1002|1003|1006)h',output): raise AssertionError('mouse reporting captured ordinary selection')
 print(json.dumps({'exit_code':p.returncode,'termios_restored':True,'mouse_capture':False}))
finally:
 if p.poll() is None:
  p.kill(); p.wait()
 os.close(master); os.close(slave)
`
	for _, mode := range []string{"dismiss", "interrupt", "start", "feedback-dismiss", "feedback"} {
		t.Run(mode, func(t *testing.T) {
			runPath, runStore, runItem := path, s, item
			if strings.HasPrefix(mode, "feedback") {
				runPath, runStore, runItem = feedbackDecisionFixture(t)
			}
			command := exec.Command("python3", "-c", program, binary, runPath, mode)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("native terminal journey: %v %s", err, output)
			}
			items, _, err := runStore.Inbox(context.Background())
			if err != nil || len(items) != 1 || items[0].ID != runItem.ID {
				t.Fatal("terminal journey lost durable item")
			}
			current := items[0]
			if mode == "start" {
				if current.State != "queued" || !current.Manual || current.Overrides["reviewer.model"] != "fixture-review-model" || current.Overrides["reviewer.reasoning_effort"] != "low" {
					t.Fatalf("terminal confirmation lost selected overrides or exact decision: %+v", current)
				}
			} else if mode == "feedback" {
				if current.State != "queued" || !current.Manual || current.Overrides["feedback.model"] != "fixture-feedback-model" || current.Overrides["feedback.reasoning_effort"] != "low" || current.Overrides["reviewer.model"] != nil {
					t.Fatalf("terminal feedback confirmation lost its independent decision: %+v", current)
				}
			} else if current.State != "pending_decision" || current.DecisionID != "" {
				t.Fatal("dismissal or interrupt granted consent")
			}
		})
	}
}
