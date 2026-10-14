//go:build darwin || linux

package source

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A finished Git leader must remain our unreaped child through group cleanup.
// Otherwise its numeric group can be reused before a later cleanup signal.
func TestGitKeepsLeaderIdentityUntilGroupCleanup(t *testing.T) {
	root := t.TempDir()
	for name, script := range map[string]string{
		"git": `#!/bin/sh
echo $$ > "$REVIEWD_TEST_LEADER"
/bin/sleep 60 </dev/null >/dev/null 2>&1 &
exit 0
`,
		"ps": `#!/bin/sh
read -r leader < "$REVIEWD_TEST_LEADER"
/bin/ps -p "$leader" -o ppid=,stat= > "$REVIEWD_TEST_OBSERVATION" || :
exec /bin/ps "$@"
`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	observation := filepath.Join(root, "observation")
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REVIEWD_TEST_LEADER", filepath.Join(root, "leader"))
	t.Setenv("REVIEWD_TEST_OBSERVATION", observation)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := runner{executable: filepath.Join(root, "git"), root: root, env: os.Environ()}
	if _, err := r.run(ctx, nil, 1024, "fixture"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(observation)
	if err != nil {
		t.Fatalf("cleanup did not retain an observable owned leader: %v", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[0] != strconv.Itoa(os.Getpid()) || !strings.HasPrefix(fields[1], "Z") {
		t.Fatalf("cleanup lost the unreaped child identity: %q", data)
	}
}

func TestAcquireRetainsSnapshotWhenShutdownCannotBeProven(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	for name, script := range map[string]string{
		"git": "#!/bin/sh\necho retained-fixture > private-canary\nexit 0\n",
		"ps":  "#!/bin/sh\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	_, err := Acquire(context.Background(), Options{Executable: filepath.Join(bin, "git"), RemoteURL: "https://github.com/owner/repo.git", TempRoot: root}, Boundary{TargetBaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Number: 1})
	if err == nil {
		t.Fatal("unavailable shutdown inspection was accepted")
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("uncertain source shutdown deleted its actual private subtree: %v %v", entries, readErr)
	}
	if _, err := os.Stat(filepath.Join(root, entries[0].Name(), "private-canary")); err != nil {
		t.Fatalf("uncertain source shutdown deleted the private canary: %v", err)
	}
}
