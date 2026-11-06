package daemon

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
)

// The web hears which list changed instead of reading them all every two
// seconds: hello on connecting, nothing while nothing changes, and the
// list's name once it does.
func TestChangesNameTheListThatChanged(t *testing.T) {
	every := changesEvery
	changesEvery = 20 * time.Millisecond
	t.Cleanup(func() { changesEvery = every })
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	_, sock := serveAPI(t, m)
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(sctx, "GET", "http://psps/v1/changes", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan string, 16)
	go func() {
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				close(events)
				return
			}
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				events <- strings.TrimSpace(name)
			}
		}
	}()
	next := func(within time.Duration) string {
		select {
		case ev := <-events:
			return ev
		case <-time.After(within):
			return ""
		}
	}
	if ev := next(5 * time.Second); ev != "hello" {
		t.Fatalf("first: %q", ev)
	}
	if ev := next(300 * time.Millisecond); ev != "" {
		t.Fatalf("nothing changed, yet %q", ev)
	}
	if _, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex}); err != nil {
		t.Fatal(err)
	}
	if ev := next(5 * time.Second); ev != "agents" {
		t.Fatalf("an agent made: %q", ev)
	}
}
