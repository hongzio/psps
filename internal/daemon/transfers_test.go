package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/isolation"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/askproxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
)

// home is where container name's /home/agent is on this Mac.
func (f *fakeContainers) home(name string) string {
	return filepath.Join(f.base, name, "home")
}

// onMac is path, a path under /home/agent in container name, on this Mac.
func (f *fakeContainers) onMac(name, path string) string {
	return filepath.Join(f.home(name), strings.TrimPrefix(path, isolation.AgentHome))
}

// transferOpen opens the file a transfer reads, refusing as the real one
// does: one that is no regular file, or is reached through a link.
func (f *fakeContainers) transferOpen(name, path string) (*os.File, error) {
	if err := isolation.TransferPath(path); err != nil {
		return nil, err
	}
	p := f.onMac(name, path)
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", path)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, err
	}
	home, _ := filepath.EvalSymlinks(f.home(name))
	if real != filepath.Join(home, strings.TrimPrefix(path, isolation.AgentHome)) {
		return nil, fmt.Errorf("reached through a link: %s", path)
	}
	return os.Open(p)
}

func (f *fakeContainers) TransferStat(_ context.Context, name, path string) (int64, string, error) {
	r, err := f.transferOpen(name, path)
	if err != nil {
		return 0, "", err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, r)
	return n, hex.EncodeToString(h.Sum(nil)), err
}

func (f *fakeContainers) Transfer(_ context.Context, from, path, to, name, part string, size int64, sum string) error {
	if err := isolation.TransferName(name); err != nil {
		return err
	}
	r, err := f.transferOpen(from, path)
	if err != nil {
		return err
	}
	defer r.Close()
	dst := filepath.Join(f.home(to), "inbox", name)
	if _, err := os.Lstat(dst); err == nil {
		return isolation.ErrTransferExists
	}
	b, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil {
		return err
	}
	if s := sha256.Sum256(b); int64(len(b)) != size || hex.EncodeToString(s[:]) != sum {
		return isolation.ErrTransferChanged
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	return os.WriteFile(dst, b, 0o644)
}

// An agent asks for a file to be moved between two other isolations,
// along a route the operator allowed; the operator approves it, and the
// file lands in the other's inbox as it was when asked for, or not at all.
func TestATransferMovesTheFileTheOperatorApproved(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, askproxy.Service{}), creds.TestStore(filepath.Join(t.TempDir(), "credentials")))
	m.tunnelPort = func(int) int { return freePort(t) }
	for _, name := range []string{"lab", "eval"} {
		if _, err := m.CreateIsolation(ctx, api.CreateIsolation{Name: name}, nil); err != nil {
			t.Fatal(err)
		}
	}
	ctr := func(name string) string {
		iso, err := m.store.IsolationNamed(name)
		if err != nil {
			t.Fatal(err)
		}
		return iso.Container
	}
	lab, eval := ctr("lab"), ctr("eval")
	bundle := []byte("a git bundle, as far as psps cares: bytes")
	put := func(path string, b []byte) {
		p := e.fc.onMac(lab, path)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("/home/agent/work/nexus.bundle", bundle)
	os.Symlink(e.fc.onMac(lab, "/home/agent/work/nexus.bundle"), e.fc.onMac(lab, "/home/agent/link.bundle"))
	sum := sha256.Sum256(bundle)
	want := hex.EncodeToString(sum[:])

	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "president"})
	e.waitFor("the agent", func() bool { return status(m, a.ID) == event.StatusIdle })
	if _, err := m.Expose(ctx, "dev", "ask", api.Expose{}); err != nil {
		t.Fatal(err)
	}
	post := func(transfer string) (int, agentAsk, string) {
		t.Helper()
		code, out := mailAs(t, m, testIsoID, a.ID, "POST", "/asks", `{"kind":"transfer","title":"Move the bundle","body":"for the runner","transfer":`+transfer+`}`)
		var v agentAsk
		json.Unmarshal([]byte(out), &v)
		return code, v, out
	}
	ok := `{"from":"lab","path":"/home/agent/work/nexus.bundle","to":"eval"}`

	// No route, none asked for.
	if code, _, out := post(ok); code != http.StatusForbidden || !strings.Contains(out, "psps transfer allow lab eval") {
		t.Fatalf("no route: %d %s", code, out)
	}
	if _, err := m.AllowRoute(api.Route{From: "lab", To: "eval"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AllowRoute(api.Route{From: "lab", To: "lab"}); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("a route to itself: %v", err)
	}
	if rs, _ := m.Routes(); len(rs) != 1 || rs[0].From != "lab" || rs[0].To != "eval" {
		t.Fatalf("routes %+v", rs)
	}
	// One way only.
	if code, _, out := post(`{"from":"eval","path":"/home/agent/x","to":"lab"}`); code != http.StatusForbidden {
		t.Fatalf("the other way: %d %s", code, out)
	}
	for _, c := range []struct {
		transfer string
		code     int
	}{
		{`{"from":"lab","path":"/etc/passwd","to":"eval"}`, http.StatusBadRequest},
		{`{"from":"lab","path":"/home/agent/work/../work/nexus.bundle","to":"eval"}`, http.StatusBadRequest},
		{`{"from":"lab","path":"/home/agent/link.bundle","to":"eval"}`, http.StatusUnprocessableEntity},
		{`{"from":"lab","path":"/home/agent/nope","to":"eval"}`, http.StatusUnprocessableEntity},
		{`{"from":"lab","path":"/home/agent/work/nexus.bundle","to":"eval","name":".bashrc"}`, http.StatusBadRequest},
		{`{"from":"lab","path":"/home/agent/work/nexus.bundle","to":"eval","sha256":"00"}`, http.StatusConflict},
		{`{"from":"nowhere","path":"/home/agent/x","to":"eval"}`, http.StatusBadRequest},
	} {
		if code, _, out := post(c.transfer); code != c.code {
			t.Errorf("%s: %d %s", c.transfer, code, out)
		}
	}
	if code, out := mailAs(t, m, testIsoID, a.ID, "POST", "/asks", `{"kind":"confirm","title":"x","transfer":`+ok+`}`); code != http.StatusBadRequest {
		t.Fatalf("a confirm with a file: %d %s", code, out)
	}

	code, x, out := post(`{"from":"lab","path":"/home/agent/work/nexus.bundle","to":"eval","sha256":"` + strings.ToUpper(want) + `"}`)
	if code != http.StatusCreated || x.Transfer == nil || x.Transfer.SHA256 != want || x.Transfer.Size != int64(len(bundle)) ||
		x.Transfer.From != "lab" || x.Transfer.To != "eval" || x.Transfer.Dest != "/home/agent/inbox/nexus.bundle" || !x.Transfer.Routed {
		t.Fatalf("posted: %d %s", code, out)
	}
	if v, _ := m.GetAsk(x.ID); v.Transfer == nil || v.Transfer.From != "lab" {
		t.Fatalf("as the operator sees it: %+v", v.Transfer)
	}
	if _, err := m.AnswerAsk(x.ID, api.AnswerAsk{Outcome: askproxy.Done}); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("done to a transfer: %v", err)
	}

	// Changed since: nothing lands, and the ask stays open.
	put("/home/agent/work/nexus.bundle", []byte("something else"))
	if _, err := m.AnswerAsk(x.ID, api.AnswerAsk{Outcome: askproxy.Yes}); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "not the one approved") {
		t.Fatalf("a changed file: %v", err)
	}
	if _, err := os.Stat(e.fc.onMac(eval, "/home/agent/inbox/nexus.bundle")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a changed file landed: %v", err)
	}
	// The route taken away meanwhile: not copied either.
	put("/home/agent/work/nexus.bundle", bundle)
	if err := m.DenyRoute(api.Route{From: "lab", To: "eval"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := m.GetAsk(x.ID); v.State != store.AskOpen || v.Transfer.Routed {
		t.Fatalf("after the route went: %+v %+v", v.Ask, v.Transfer)
	}
	if _, err := m.AnswerAsk(x.ID, api.AnswerAsk{Outcome: askproxy.Yes}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("no route: %v", err)
	}
	m.AllowRoute(api.Route{From: "lab", To: "eval"})

	v, err := m.AnswerAsk(x.ID, api.AnswerAsk{Outcome: askproxy.Yes, Text: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != store.AskAnswered || v.Transfer.CopiedAt == nil || v.Verdict != "approved: the file is copied" {
		t.Fatalf("approved: %+v %+v", v.Ask, v.Transfer)
	}
	if got, err := os.ReadFile(e.fc.onMac(eval, "/home/agent/inbox/nexus.bundle")); err != nil || !bytes.Equal(got, bundle) {
		t.Fatalf("landed: %q %v", got, err)
	}
	stored, _ := m.store.GetAsk(x.ID)
	if s := askproxy.Answer(stored); !strings.Contains(s, "Copied: /home/agent/inbox/nexus.bundle") || !strings.Contains(s, want) {
		t.Fatalf("what the agent hears: %s", s)
	}

	// Never over a file in the inbox.
	_, y, _ := post(ok)
	if _, err := m.AnswerAsk(y.ID, api.AnswerAsk{Outcome: askproxy.Yes}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("over a file there: %v", err)
	}
	if v, err := m.AnswerAsk(y.ID, api.AnswerAsk{Outcome: askproxy.No}); err != nil || v.Verdict != "declined" {
		t.Fatalf("declined: %+v %v", v, err)
	}

	// Removing an isolation takes its routes with it.
	if err := m.store.DeleteIsolation(m.mustIso(t, "lab")); err != nil {
		t.Fatal(err)
	}
	if rs, _ := m.Routes(); len(rs) != 0 {
		t.Fatalf("routes left: %+v", rs)
	}
}

func (m *Manager) mustIso(t *testing.T, name string) string {
	iso, err := m.store.IsolationNamed(name)
	if err != nil {
		t.Fatal(err)
	}
	return iso.ID
}
