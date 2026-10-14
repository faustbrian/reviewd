package cli

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/faustbrian/reviewd/internal/clarification"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestInteractiveClarificationExplicitAnswerAndTerminalRestoration(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "reviewd")
	build := exec.Command("go", "build", "-o", binary, "./cmd/reviewd")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("native CLI build: %v %s", err, output)
	}
	program := `
import errno,fcntl,json,os,re,select,signal,struct,subprocess,sys,termios,time
binary,config,run,mode=sys.argv[1:]
master,slave=os.openpty();original=termios.tcgetattr(slave)
fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',26,100,0,0))
env=os.environ.copy();env['TERM']='xterm-256color'
p=subprocess.Popen([binary,'--config',config,'inbox','--interactive'],stdin=slave,stdout=slave,stderr=slave,env=env,start_new_session=True)
output=bytearray()
def wait_for(text):
 deadline=time.monotonic()+8
 while text.encode() not in output:
  if time.monotonic()>deadline or p.poll() is not None: raise AssertionError('missing '+repr(text)+': '+repr(bytes(output)[-1400:]))
  ready,_,_=select.select([master],[],[],.1)
  if ready:
   try:output.extend(os.read(master,65536))
   except OSError as e:
    if e.errno!=errno.EIO:raise
def send(data):os.write(master,data)
try:
 wait_for('Clarification');send(b'3');time.sleep(.1);send(b'j');send(b'i')
 wait_for('Answer exact private clarification');wait_for('Question 1/2')
 send(b'Billing\r');wait_for('2/2 · Private');send(b'secret-pty-canary')
 time.sleep(.15)
 fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',18,80,0,0));os.kill(p.pid,signal.SIGWINCH)
 if mode=='interrupt':send(b'\x03')
 elif mode=='dismiss':send(b'\x1b');time.sleep(.15);send(b'q')
 else:
  send(b'\r');wait_for('Answers captured privately')
  if mode=='stale':
   result=subprocess.run([binary,'--config',config,'cancel',run,'--json'],capture_output=True,timeout=5)
   if result.returncode:raise AssertionError('owned cancellation failed')
  send(b'y');wait_for('Answer selected locally' if mode=='answer' else 'Action not completed')
  send(b'q')
 deadline=time.monotonic()+8
 while p.poll() is None:
  if time.monotonic()>deadline:raise AssertionError('terminal did not exit while PTY output was being drained')
  ready,_,_=select.select([master],[],[],.05)
  if ready:
   try:output.extend(os.read(master,65536))
   except OSError as e:
    if e.errno!=errno.EIO:raise
 p.wait(timeout=1)
 if p.returncode!=(130 if mode=='interrupt' else 0):raise AssertionError('unexpected terminal exit '+str(p.returncode))
 restored=termios.tcgetattr(slave)
 original[3]&=~getattr(termios,'PENDIN',0);restored[3]&=~getattr(termios,'PENDIN',0)
 if restored!=original:raise AssertionError('terminal modes not restored')
 if b'secret-pty-canary' in output:raise AssertionError('secret editor text echoed')
 if re.search(rb'\x1b\[\?(1000|1002|1003|1006)h',output):raise AssertionError('mouse capture enabled')
 print(json.dumps({'exit_code':p.returncode,'termios_restored':True,'secret_masked':True,'mouse_capture':False}))
finally:
 if p.poll() is None:p.kill();p.wait()
 os.close(master);os.close(slave)
`
	for _, mode := range []string{"answer", "dismiss", "interrupt", "stale"} {
		t.Run(mode, func(t *testing.T) {
			path, s, _ := terminalFixture(t)
			ctx := context.Background()
			run, _, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 702, Number: 18, ActorID: 43, ActorLogin: "reviewer", WorkKey: "input-pty", State: store.Queued, Configuration: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Claim(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.RecordProgress(ctx, run.ID, "pty-thread", "pty-turn", "fixture-model", "fixture-model", "waiting_input", nil); err != nil {
				t.Fatal(err)
			}
			request := clarification.Request{ID: "input-pty", ThreadID: "pty-thread", TurnID: "pty-turn", ItemID: "item", WireID: `"input"`, Participant: "primary", Transport: "dynamic", Questions: []clarification.Question{{ID: "scope", Header: "Scope", Question: "Which variant?"}, {ID: "private", Header: "Private", Question: "Private token?", IsSecret: true}}}
			if err = s.OpenInput(ctx, run.ID, request); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("python3", "-c", program, binary, path, run.ID, mode)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("native clarification editor: %v %s", err, output)
			}
			inputs, err := s.Inputs(ctx, run.ID)
			if err != nil || len(inputs) != 1 {
				t.Fatal("terminal lost input history")
			}
			want := "pending"
			if mode == "answer" {
				want = "selected"
			}
			if inputs[0].State != want {
				t.Fatalf("terminal %s unexpectedly changed consent to %s", mode, inputs[0].State)
			}
			if mode == "answer" {
				answer, err := s.WaitInput(ctx, request.ID)
				if err != nil || answer.Answers["scope"].Answers[0] != "Billing" || answer.Answers["private"].Answers[0] != "secret-pty-canary" {
					t.Fatal("terminal altered the exact selected answer")
				}
			}
		})
	}
}
