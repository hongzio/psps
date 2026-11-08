package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/shim"
	"github.com/hongzio/psps/internal/store"
)

// hostShim serves a shim where the daemon looks for the Mac's, in this
// process, as `psps host-shim` would.
func (e *testEnv) hostShim(m *Manager) {
	e.t.Helper()
	m.hostRoot = filepath.Join(e.fc.base, "host")
	srv := &shim.Server{Root: m.hostRoot, BaseEnv: []string{"PATH=" + os.Getenv("PATH")}}
	if err := srv.Open(); err != nil {
		e.t.Fatal(err)
	}
	l, err := net.Listen("unix", m.hostSocket())
	if err != nil {
		e.t.Fatal(err)
	}
	go srv.Serve(l)
	e.t.Cleanup(func() { l.Close(); srv.Shutdown(0) })
}

func TestAdminModeRunsAnAgentOnTheMac(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	e.hostShim(m)
	dir := t.TempDir()

	if v, err := m.Admin(); err != nil || v.On {
		t.Fatalf("admin mode before it is turned on: %+v, %v", v, err)
	}
	if _, err := m.SetAdmin(ctx, api.SetAdmin{Kind: driver.Codex, Dir: filepath.Join(dir, "nowhere")}); err == nil {
		t.Fatal("admin mode came on in a directory that is not there")
	}
	v, err := m.SetAdmin(ctx, api.SetAdmin{Kind: driver.Codex, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !v.On || v.Kind != driver.Codex || v.Dir != dir || len(v.Recent) != 1 || v.Recent[0] != dir {
		t.Fatalf("admin mode on: %+v", v)
	}
	a, err := m.Agent(v.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Host || a.IsolationID != store.HostIsolation || a.Workdir != dir || a.Name != "Admin" || a.Handle != "admin" {
		t.Fatalf("the admin agent: %+v", a)
	}
	if a.Options.Approvals != driver.ApprovalsDefault {
		t.Fatalf("the admin agent's approvals are %q, not the CLI's own", a.Options.Approvals)
	}

	// The host is no isolation people see or name.
	isos, err := m.Isolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range isos {
		if i.Name == store.HostIsolation {
			t.Fatal("the host is listed among the isolations")
		}
	}
	if _, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: store.HostIsolation, Kind: driver.Codex}); err == nil {
		t.Fatal("an agent was made on the host as in an isolation")
	}
	if _, err := m.Fork(ctx, a.ID); err == nil {
		t.Fatal("the admin agent was forked")
	}

	// It works like any agent, its process the host shim's.
	if err := m.SetOptions(a.ID, driver.Options{Approvals: driver.ApprovalsNever}); err != nil {
		t.Fatal(err)
	}
	if err := m.Send(a.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the admin agent's answer", func() bool { return e.hasAssistant(a.ID, "echo: hello") && status(m, a.ID) == event.StatusIdle })
	if _, err := os.Stat(filepath.Join(m.hostRoot, "agents", a.ID)); err != nil {
		t.Fatalf("its log is not the host shim's: %v", err)
	}

	// The same again keeps it; another directory starts it over.
	if again, err := m.SetAdmin(ctx, api.SetAdmin{Kind: driver.Codex, Dir: dir}); err != nil || again.Agent != a.ID {
		t.Fatalf("turning it on as it is: %+v, %v", again, err)
	}
	other := t.TempDir()
	v2, err := m.SetAdmin(ctx, api.SetAdmin{Kind: driver.Codex, Dir: other})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Agent == a.ID || v2.Dir != other || len(v2.Recent) != 2 || v2.Recent[0] != other {
		t.Fatalf("started over: %+v", v2)
	}
	if old, _ := e.st.Agent(a.ID); old.ArchivedAt == nil {
		t.Fatal("the admin agent it replaced is not archived")
	}
	if _, err := m.Restore(ctx, a.ID); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("an old admin agent was restored beside the one running: %v", err)
	}

	off, err := m.StopAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if off.On {
		t.Fatalf("admin mode still on: %+v", off)
	}
	if cur, _ := e.st.Agent(v2.Agent); cur.ArchivedAt == nil {
		t.Fatal("turning admin mode off left its agent live")
	}

	// Deleted, its log goes from the Mac.
	e.waitFor("its process gone", func() bool { return m.DeleteAgent(ctx, a.ID, false) == nil })
	if _, err := os.Stat(filepath.Join(m.hostRoot, "agents", a.ID)); !os.IsNotExist(err) {
		t.Fatalf("the deleted admin agent's log is still on the Mac: %v", err)
	}
}

func TestHostDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{"work/psps", "work/play", "work/.hidden", "Documents"} {
		os.MkdirAll(filepath.Join(home, d), 0o755)
	}
	os.WriteFile(filepath.Join(home, "work", "pfile"), nil, 0o644)
	m := &Manager{}
	ds, err := m.HostDirs("~/work/p")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{home + "/work/play/", home + "/work/psps/"}
	if strings.Join(ds.Dirs, ",") != strings.Join(want, ",") || ds.Home != home {
		t.Fatalf("dirs: %+v, want %v", ds, want)
	}
	if ds, _ = m.HostDirs("~/work/"); len(ds.Dirs) != 2 {
		t.Fatalf("a hidden directory was offered before a dot was typed: %v", ds.Dirs)
	}
	if ds, _ = m.HostDirs(""); len(ds.Dirs) != 2 || ds.Parent != home+"/" {
		t.Fatalf("nothing typed is the home: %+v", ds)
	}
	if _, err := m.HostDirs("relative"); err == nil {
		t.Fatal("a relative path was completed")
	}
	if d, err := hostDir("~/work/psps/"); err != nil || d != home+"/work/psps" {
		t.Fatalf("hostDir: %q, %v", d, err)
	}
}
