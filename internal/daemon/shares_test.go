package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/shareproxy"
)

// shareEnv is the test isolation with share as its one service, over a
// home folder of its own.
func shareEnv(t *testing.T) (*testEnv, *Manager, string) {
	t.Helper()
	e := newTestEnv(t)
	m, stop := e.daemon()
	t.Cleanup(stop)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	for _, d := range []string{"notes", "out", "denied"} {
		os.MkdirAll(filepath.Join(home, d), 0o755)
	}
	m.SetProxies(proxy.NewRegistry(shareproxy.Service{Home: home, Protected: []string{}}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	return e, m, home
}

func (e *testEnv) mounted() []isolation.Mount {
	e.fc.mu.Lock()
	defer e.fc.mu.Unlock()
	return e.fc.shares[testCtr]
}

// gone removes the container under the daemon, which notices on its next
// look rather than from its last good one.
func (e *testEnv) gone(m *Manager) {
	e.fc.Remove(context.Background(), testCtr)
	os.RemoveAll(filepath.Join(e.fc.base, testCtr))
	e.fc.mu.Lock()
	delete(e.fc.shares, testCtr)
	e.fc.mu.Unlock()
	st := m.isoLock(testIsoID)
	st.mu.Lock()
	st.healthy = time.Time{}
	st.mu.Unlock()
}

func (e *testEnv) creates() int {
	e.fc.mu.Lock()
	defer e.fc.mu.Unlock()
	return len(e.fc.created)
}

func TestAShareIsMountedByReplacingTheContainer(t *testing.T) {
	e, m, home := shareEnv(t)
	ctx := context.Background()
	notes := filepath.Join(home, "notes")
	if _, err := m.PutEnv(ctx, "share", "notes", proxy.Patch{Set: map[string]any{"path": notes}}, true); err != nil {
		t.Fatal(err)
	}
	if e.creates() != 0 {
		t.Fatal("defining a share touched a container")
	}
	if _, err := m.Expose(ctx, "dev", "share", api.Expose{Envs: []string{"notes"}}); err != nil {
		t.Fatal(err)
	}
	if got := e.mounted(); len(got) != 1 || got[0] != (isolation.Mount{Source: notes, Target: "/shared/notes", ReadOnly: true}) {
		t.Fatalf("mounted %+v", got)
	}
	// The skill says what the folder is.
	req, _ := e.fc.lastSync()
	var skill string
	for _, f := range req.Files {
		if strings.HasSuffix(f.Path, "gero-share/SKILL.md") {
			skill = f.Content
		}
	}
	if !strings.Contains(skill, "/shared/notes is the Mac's "+notes+", read-only") {
		t.Fatalf("skill:\n%s", skill)
	}
	// Exposed as it already is: the container stays.
	n := e.creates()
	if _, err := m.Expose(ctx, "dev", "share", api.Expose{Envs: []string{"notes"}}); err != nil || e.creates() != n {
		t.Fatalf("an unchanged share replaced the container: %v", err)
	}

	// Verify measures it from inside, read-only as shared.
	v, err := m.Verify(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if cs := checkNamed(v, "no Mac directory mounted but its shares"); len(cs) != 1 || !cs[0].OK || cs[0].Got != "/shared/notes (read-only)" {
		t.Fatalf("verify: %+v", cs)
	}
	if cs := checkNamed(v, "its shares are mounted"); len(cs) != 1 || !cs[0].OK {
		t.Fatalf("verify: %+v", cs)
	}

	// Made writable: the env edit replaces it, and says what that means.
	env, err := m.PutEnv(ctx, "share", "notes", proxy.Patch{Set: map[string]any{"read_write": true}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "read-write") {
		t.Fatalf("warnings %v", env.Warnings)
	}
	if got := e.mounted(); len(got) != 1 || got[0].ReadOnly {
		t.Fatalf("after read-write: %+v", got)
	}

	// Off: replaced without it.
	if err := m.Unexpose(ctx, "dev", "share"); err != nil {
		t.Fatal(err)
	}
	if got := e.mounted(); len(got) != 0 {
		t.Fatalf("after unexpose: %+v", got)
	}
	v, _ = m.Verify(ctx, "dev")
	if cs := checkNamed(v, "no Mac directory mounted in the isolation"); len(cs) != 1 || !cs[0].OK {
		t.Fatalf("verify without shares: %+v", cs)
	}
}

// A share the container will not start with (macOS keeping the runtime
// out of it) is not exposed: the container comes back with what it had.
func TestAShareTheContainerRefusesIsTakenBack(t *testing.T) {
	e, m, home := shareEnv(t)
	ctx := context.Background()
	for _, n := range []string{"notes", "denied"} {
		if _, err := m.PutEnv(ctx, "share", n, proxy.Patch{Set: map[string]any{"path": filepath.Join(home, n)}}, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Expose(ctx, "dev", "share", api.Expose{Envs: []string{"notes"}}); err != nil {
		t.Fatal(err)
	}
	e.fc.mu.Lock()
	e.fc.refuseShare = filepath.Join(home, "denied")
	e.fc.mu.Unlock()
	_, err := m.Expose(ctx, "dev", "share", api.Expose{Envs: []string{"notes", "denied"}})
	if err == nil || !strings.Contains(err.Error(), "Operation not permitted") || !strings.Contains(err.Error(), "back with the folders it had") {
		t.Fatalf("%v", err)
	}
	if got := e.mounted(); len(got) != 1 || got[0].Target != "/shared/notes" {
		t.Fatalf("mounted %+v", got)
	}
	if x, _ := e.st.Exposure(testIsoID, "share"); strings.Join(x.Envs, ",") != "notes" {
		t.Fatalf("the exposure kept what does not run: %v", x.Envs)
	}
}

// A container that is gone comes back with its shares, without one whose
// folder has gone, and without them all if it will not start with them.
func TestARecreatedContainerHasItsShares(t *testing.T) {
	e, m, home := shareEnv(t)
	ctx := context.Background()
	for _, n := range []string{"notes", "out"} {
		if _, err := m.PutEnv(ctx, "share", n, proxy.Patch{Set: map[string]any{"path": filepath.Join(home, n)}}, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Expose(ctx, "dev", "share", api.Expose{Envs: []string{"notes", "out"}}); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(home, "out"))
	e.gone(m)
	if _, err := m.ensureIsolation(ctx, testIsoID); err != nil {
		t.Fatal(err)
	}
	if got := e.mounted(); len(got) != 1 || got[0].Target != "/shared/notes" {
		t.Fatalf("mounted %+v", got)
	}
	v, _ := m.Verify(ctx, "dev")
	if cs := checkNamed(v, "its shares are mounted"); len(cs) != 1 || cs[0].OK || !strings.Contains(cs[0].Got, "out (") {
		t.Fatalf("verify: %+v", cs)
	}

	e.fc.mu.Lock()
	e.fc.refuseShare = filepath.Join(home, "notes")
	e.fc.mu.Unlock()
	e.gone(m)
	if _, err := m.ensureIsolation(ctx, testIsoID); err != nil {
		t.Fatal(err)
	}
	if got := e.mounted(); len(got) != 0 {
		t.Fatalf("mounted %+v", got)
	}
	v, _ = m.Verify(ctx, "dev")
	if cs := checkNamed(v, "its shares are mounted"); len(cs) != 1 || cs[0].OK || cs[0].Got != "not mounted" {
		t.Fatalf("verify: %+v", cs)
	}
}

// What the isolation sees is measured against its shares: a writable mount
// where the share is read-only, or a folder no share names, is a breach.
func TestVerifyFailsAMountItsSharesDoNotName(t *testing.T) {
	e, m, home := shareEnv(t)
	ctx := context.Background()
	if _, err := m.PutEnv(ctx, "share", "notes", proxy.Patch{Set: map[string]any{"path": filepath.Join(home, "notes")}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Expose(ctx, "dev", "share", api.Expose{Envs: []string{"notes"}}); err != nil {
		t.Fatal(err)
	}
	e.fc.mu.Lock()
	e.fc.mounts = "/dev/vdb / ext4 rw 0 0\nvirtiofs /shared/notes virtiofs rw,relatime 0 0\nvirtiofs /mnt/mac virtiofs rw 0 0"
	e.fc.mu.Unlock()
	v, err := m.Verify(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	cs := checkNamed(v, "no Mac directory mounted but its shares")
	if len(cs) != 1 || cs[0].OK || cs[0].Severity != api.SevIsolation || cs[0].Got != "/shared/notes is writable, shared read-only, /mnt/mac (virtiofs)" {
		t.Fatalf("%+v", cs)
	}
	if v.ExitCode() != api.VerifyIsolation {
		t.Fatalf("exit code %d", v.ExitCode())
	}
}
