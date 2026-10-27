package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/client"
)

// fakeLoginDaemon answers the two login calls the way the daemon does and
// records what came back.
type fakeLoginDaemon struct {
	mu        sync.Mutex
	redirect  string
	callbacks []string
}

func (d *fakeLoginDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/login"):
		var req api.LoginStart
		json.NewDecoder(r.Body).Decode(&req)
		d.redirect = req.Redirect
		json.NewEncoder(w).Encode(api.Login{ID: "id1", URL: "https://login.example/authorize?state=s1", Redirect: req.Redirect, Expires: time.Now().Add(time.Minute)})
	case strings.HasSuffix(r.URL.Path, "/login/id1"):
		var req api.LoginFinish
		json.NewDecoder(r.Body).Decode(&req)
		d.callbacks = append(d.callbacks, req.Callback)
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}

func loginDaemon(t *testing.T) (*fakeLoginDaemon, *client.Client) {
	t.Helper()
	// A unix socket path must stay under 104 bytes; t.TempDir's does not.
	dir, err := os.MkdirTemp("", "gl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeLoginDaemon{}
	srv := &http.Server{Handler: d}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return d, client.New(sock)
}

func TestABrowserOnThisMacComesBackByItself(t *testing.T) {
	d, c := loginDaemon(t)
	in, _ := io.Pipe() // nobody types
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- browserLogin(t.Context(), c, "notion", "work", in, &syncWriter{w: &out}) }()
	var redirect string
	for deadline := time.Now().Add(5 * time.Second); redirect == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		d.mu.Lock()
		redirect = d.redirect
		d.mu.Unlock()
	}
	u, err := url.Parse(redirect)
	if err != nil || u.Hostname() != "127.0.0.1" {
		t.Fatalf("redirect %q", redirect)
	}
	// The browser follows the provider's redirect.
	resp, err := http.Get(redirect + "?code=c1&state=s1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(d.callbacks) != 1 || d.callbacks[0] != "?code=c1&state=s1" {
		t.Fatalf("%v", d.callbacks)
	}
}

func TestAnAddressPastedFromAnotherDeviceFinishesTheLogin(t *testing.T) {
	d, c := loginDaemon(t)
	pasted := "http://127.0.0.1:61000/callback?code=c2&state=s1\n"
	var out strings.Builder
	if err := browserLogin(t.Context(), c, "notion", "work", strings.NewReader("\n"+pasted), &syncWriter{w: &out}); err != nil {
		t.Fatal(err)
	}
	if len(d.callbacks) != 1 || d.callbacks[0] != strings.TrimSpace(pasted) {
		t.Fatalf("%v", d.callbacks)
	}
	if !strings.Contains(out.String(), "https://login.example/authorize?state=s1") || !strings.Contains(out.String(), "paste it here") {
		t.Fatalf("%s", out.String())
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
