package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
)

func TestDoctorReportsMissingMacFeedbackBackendWithoutDisablingReviews(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS container requirement")
	}
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	if err := os.WriteFile(path, []byte("schema_version=1\n[feedback]\nauthors=['@me']\naction='auto'\n[state]\ndirectory='"+filepath.ToSlash(root)+"/state'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Error("doctor attempted PR execution")
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"id":42,"login":"fixture"}`)
	}))
	defer peer.Close()
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		return github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	}, Codex: func(context.Context, []config.Snapshot) []Diagnostic { return nil }}
	var out, diagnostics bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--config", path, "--json"}, &out, &diagnostics, deps); code != 0 {
		t.Fatalf("optional feedback dependency disabled ordinary reviewing: %d %s", code, diagnostics.String())
	}
	var report diagnosticReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Ready {
		t.Fatal("ordinary review prerequisites were marked unavailable")
	}
	for _, d := range report.Diagnostics {
		if d.Check == "feedback command backend" {
			if d.Status != "warning" || !strings.Contains(d.Correction, "feedback.container_endpoint") || !strings.Contains(d.Message, "not connected") {
				t.Fatal("feedback availability was overstated or correction was not actionable")
			}
			return
		}
	}
	t.Fatal("doctor hid unavailable Mac modifying feedback backend")
}
