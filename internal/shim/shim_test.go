package shim

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/shimproto"
)

// echoAgent answers every stdin line and exits 3 on "exit".
var echoAgent = []string{"/bin/sh", "-c", `while IFS= read -r l; do [ "$l" = exit ] && exit 3; echo "got:$l"; done`}

type harness struct {
	t      *testing.T
	root   string
	srv    *Server
	l      net.Listener
	client *Client
}

func newHarness(t *testing.T, root string) *harness {
	t.Helper()
	if root == "" {
		var err error
		// Short path: macOS caps unix socket paths at 104 bytes.
		root, err = os.MkdirTemp("", "gs")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(root) })
	}
	srv := &Server{Root: root, BaseEnv: []string{"PATH=/usr/bin:/bin"}}
	if err := srv.Open(); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "shim.sock")
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	h := &harness{t: t, root: root, srv: srv, l: l, client: &Client{Dial: UnixDialer(sock)}}
	t.Cleanup(func() { l.Close(); srv.Shutdown(2 * time.Second) })
	return h
}

func (h *harness) attach(id string, after int64) *Attachment {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	a, err := h.client.Attach(ctx, id, after)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

// next waits for the next record that matches.
func next(t *testing.T, a *Attachment, match func(shimproto.Record) bool) shimproto.Record {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case rec, ok := <-a.Records:
			if !ok {
				t.Fatalf("attachment closed: %v", a.Err())
			}
			if match(rec) {
				return rec
			}
		case <-deadline:
			t.Fatal("timed out waiting for record")
		}
	}
}

func isSys(kind shimproto.SysKind) func(shimproto.Record) bool {
	return func(r shimproto.Record) bool { return r.Dir == shimproto.DirSys && r.Sys.Kind == kind }
}

func isLine(dir shimproto.Dir, line string) func(shimproto.Record) bool {
	return func(r shimproto.Record) bool { return r.Dir == dir && r.Line == line }
}

func TestStartAttachWriteAndExit(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()

	att := h.attach("a1", 0)
	if att.LastSeq != 0 {
		t.Fatalf("fresh agent LastSeq = %d", att.LastSeq)
	}
	if _, err := h.client.Start(ctx, "a1", shimproto.Spec{Argv: echoAgent}); err != nil {
		t.Fatal(err)
	}
	started := next(t, att, isSys(shimproto.SysStarted))
	if err := att.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	in := next(t, att, isLine(shimproto.DirIn, "hello"))
	out := next(t, att, isLine(shimproto.DirOut, "got:hello"))
	if !(started.Seq < in.Seq && in.Seq < out.Seq) {
		t.Fatalf("records out of order: started=%d in=%d out=%d", started.Seq, in.Seq, out.Seq)
	}

	// A second start while running is refused.
	if _, err := h.client.Start(ctx, "a1", shimproto.Spec{Argv: echoAgent}); err == nil {
		t.Fatal("second start succeeded")
	}

	att.Write([]byte("exit"))
	exited := next(t, att, isSys(shimproto.SysExited))
	if exited.Sys.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", exited.Sys.ExitCode)
	}
}

func TestAttachReplaysFromAfter(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	att := h.attach("a1", 0)
	h.client.Start(ctx, "a1", shimproto.Spec{Argv: echoAgent})
	att.Write([]byte("one"))
	mark := next(t, att, isLine(shimproto.DirOut, "got:one"))
	att.Write([]byte("two"))
	next(t, att, isLine(shimproto.DirOut, "got:two"))
	att.Close()

	// A reader that saw everything up to "got:one" gets exactly the rest,
	// starting with the "in" record for "two".
	again := h.attach("a1", mark.Seq)
	first := next(t, again, func(shimproto.Record) bool { return true })
	if first.Seq != mark.Seq+1 || first.Dir != shimproto.DirIn || first.Line != "two" {
		t.Fatalf("first replayed record = %+v, want seq %d in:two", first, mark.Seq+1)
	}
	if again.LastSeq < first.Seq {
		t.Fatalf("LastSeq %d precedes replayed record %d", again.LastSeq, first.Seq)
	}
}

func TestRestartRecordsLostAndKeepsSeq(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	att := h.attach("a1", 0)
	h.client.Start(ctx, "a1", shimproto.Spec{Argv: echoAgent})
	started := next(t, att, isSys(shimproto.SysStarted))
	att.Close()
	h.l.Close()
	t.Cleanup(func() { syscallKill(started.Sys.PID) })

	// A new shim on the same root has no process for a1, which state.json
	// still calls running: that is a lost agent.
	h2 := newHarness(t, h.root)
	again := h2.attach("a1", started.Seq)
	lost := next(t, again, isSys(shimproto.SysLost))
	if lost.Seq != started.Seq+1 {
		t.Fatalf("lost seq = %d, want %d", lost.Seq, started.Seq+1)
	}
	infos, err := h2.client.List(ctx)
	if err != nil || len(infos) != 1 || infos[0].Running {
		t.Fatalf("list after restart = %+v, %v", infos, err)
	}

	// The next generation continues the same sequence.
	h2.client.Start(ctx, "a1", shimproto.Spec{Argv: echoAgent})
	restarted := next(t, again, isSys(shimproto.SysStarted))
	if restarted.Seq <= lost.Seq {
		t.Fatalf("restart seq %d not after lost %d", restarted.Seq, lost.Seq)
	}
}

func TestTornTailIsCut(t *testing.T) {
	root, _ := os.MkdirTemp("", "gs")
	t.Cleanup(func() { os.RemoveAll(root) })
	dir := filepath.Join(root, "agents", "a1")
	os.MkdirAll(dir, 0o700)
	good := `{"seq":1,"t":"2026-01-01T00:00:00Z","dir":"out","line":"x"}` + "\n"
	os.WriteFile(filepath.Join(dir, "log.jsonl"), []byte(good+`{"seq":2,"t":"20`), 0o600)

	h := newHarness(t, root)
	att := h.attach("a1", 0)
	if att.LastSeq != 1 {
		t.Fatalf("LastSeq = %d, want 1", att.LastSeq)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if string(b) != good {
		t.Fatalf("log not truncated to the good prefix: %q", b)
	}
}

func TestInvalidAgentID(t *testing.T) {
	h := newHarness(t, "")
	if _, err := h.client.Attach(context.Background(), "../etc", 0); err == nil {
		t.Fatal("path-like agent id accepted")
	}
}
