package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/client"
	"github.com/hongzio/gero/internal/driver"
)

// fakeCLIDaemon runs the daemon's side of an agent CLI login: Claude's
// takes "good#abc", Codex's ends after polls GETs.
type fakeCLIDaemon struct {
	mu        sync.Mutex
	tool      string
	codes     []string
	cancelled bool
	polls     int
}

func (d *fakeCLIDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	open := api.CLILogin{Tool: driver.Kind(d.tool), State: api.CLILoginOpen, URL: "https://claude.com/cai/oauth/authorize?state=abc", NeedsCode: true,
		Expires: time.Now().Add(15 * time.Minute)}
	if d.tool == "codex" {
		open.URL, open.Code, open.NeedsCode = "https://auth.openai.com/codex/device", "LK6M-R8WQK", false
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/isolations/dev/clis/"+d.tool)
	switch {
	case r.Method == "POST" && path == "/login":
		json.NewEncoder(w).Encode(open)
	case r.Method == "POST" && path == "/login/code":
		var req api.CLICode
		json.NewDecoder(r.Body).Decode(&req)
		d.codes = append(d.codes, req.Code)
		l := api.CLILogin{Tool: driver.Claude, State: api.CLILoginDone, Message: "Login successful."}
		if strings.TrimSpace(req.Code) != "good#abc" {
			l.State, l.Message = api.CLILoginFailed, "Login failed: That code wasn't accepted."
		}
		json.NewEncoder(w).Encode(l)
	case r.Method == "GET" && path == "/login":
		d.polls++
		if d.polls >= 2 {
			open = api.CLILogin{Tool: driver.Codex, State: api.CLILoginDone, Message: "Successfully logged in"}
		}
		json.NewEncoder(w).Encode(open)
	case r.Method == "DELETE" && path == "/login":
		d.cancelled = true
		w.Write([]byte(`{"ok":true}`))
	default:
		http.NotFound(w, r)
	}
}

func cliDaemon(t *testing.T, tool string) (*fakeCLIDaemon, *client.Client) {
	t.Helper()
	dir, err := os.MkdirTemp("", "gc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeCLIDaemon{tool: tool}
	srv := &http.Server{Handler: d}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return d, client.New(filepath.Join(dir, "s"))
}

func TestClaudeLoginHandsBackThePastedCode(t *testing.T) {
	d, c := cliDaemon(t, "claude")
	var out strings.Builder
	if err := loginCLI(t.Context(), c, "dev", "claude", strings.NewReader("good#abc\n"), &out); err != nil {
		t.Fatal(err)
	}
	if len(d.codes) != 1 || d.codes[0] != "good#abc\n" || d.cancelled {
		t.Fatalf("codes %q cancelled %v", d.codes, d.cancelled)
	}
	for _, want := range []string{"https://claude.com/cai/oauth/authorize?state=abc", "Paste it here", "Login successful.", "claude is logged in in dev"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}

	d, c = cliDaemon(t, "claude")
	err := loginCLI(t.Context(), c, "dev", "claude", strings.NewReader("bad\n"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "That code wasn't accepted") {
		t.Fatalf("%v", err)
	}

	// Nothing pasted: the login inside is stopped, not left waiting.
	d, c = cliDaemon(t, "claude")
	if err := loginCLI(t.Context(), c, "dev", "claude", strings.NewReader(""), io.Discard); err == nil || !d.cancelled {
		t.Fatalf("err %v cancelled %v", err, d.cancelled)
	}
}

func TestCodexLoginShowsTheCodeAndWaits(t *testing.T) {
	d, c := cliDaemon(t, "codex")
	var out strings.Builder
	if err := loginCLI(t.Context(), c, "dev", "codex", strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if d.polls < 2 || d.cancelled {
		t.Fatalf("polls %d cancelled %v", d.polls, d.cancelled)
	}
	for _, want := range []string{"https://auth.openai.com/codex/device", "LK6M-R8WQK", "codex is logged in in dev"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
}
