package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/cron"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/cronproxy"
	"github.com/hongzio/gero/internal/proxy/mailproxy"
	"github.com/hongzio/gero/internal/store"
)

// An agent sets, changes and removes crons of its own at its address; it
// sees its operator's and leaves them be, and keeps within its limits.
func TestAnAgentKeepsCronsOfItsOwn(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, cronproxy.Service{}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	m.tunnelPort = func(int) int { return freePort(t) }
	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "alice"})
	e.waitFor("the agent", func() bool { return status(m, a.ID) == event.StatusIdle })
	stored, _ := m.store.Agent(a.ID)

	// Off: no address, and the port refuses.
	if env := m.cronEnv(stored); env != nil {
		t.Fatalf("an address while off: %q", env)
	}
	if code, out := mailAs(t, m, testIsoID, a.ID, "GET", "/crons", ""); code != http.StatusForbidden || !strings.Contains(out, "gero iso expose dev cron") {
		t.Fatalf("off: %d %s", code, out)
	}
	if _, err := m.Expose(ctx, "dev", "cron", api.Expose{}); err != nil {
		t.Fatal(err)
	}
	tok, _ := m.store.MailToken(a.ID)
	if env := m.cronEnv(stored); len(env) != 1 || env[0] != "GERO_CRON_URL="+cronproxy.URL(tok) {
		t.Fatalf("env %q", env)
	}
	if code, out := mailAt(m, testIsoID, "GET", "/a/nope/crons", ""); code != http.StatusForbidden {
		t.Fatalf("a wrong token: %d %s", code, out)
	}

	ops, err := m.AddCron(a.ID, api.PutCron{Schedule: ptr("@daily"), Prompt: ptr("the operator's")})
	if err != nil {
		t.Fatal(err)
	}
	code, out := mailAs(t, m, testIsoID, a.ID, "POST", "/crons", `{"schedule":"@every 1h","prompt":"check CI","if_busy":"wait"}`)
	var mine store.Cron
	json.Unmarshal([]byte(out), &mine)
	if code != http.StatusCreated || !mine.Self || mine.Schedule != "@every 1h" || mine.IfBusy != store.CronWait || mine.NextAt == nil {
		t.Fatalf("set: %d %s", code, out)
	}
	code, out = mailAs(t, m, testIsoID, a.ID, "GET", "/crons", "")
	var list api.Crons
	json.Unmarshal([]byte(out), &list)
	if code != http.StatusOK || len(list.Crons) != 2 || list.Crons[0].Self || !list.Crons[1].Self || list.Zone == "" {
		t.Fatalf("list: %d %s", code, out)
	}

	// Too often, or not read: refused, as is the operator's cron.
	for _, body := range []string{`{"schedule":"@every 1m","prompt":"x"}`, `{"schedule":"*/2 * * * *","prompt":"x"}`,
		`{"schedule":"0,3 9 * * *","prompt":"x"}`, `{"schedule":"soon","prompt":"x"}`, `{"prompt":"x"}`, `not json`} {
		if code, out := mailAs(t, m, testIsoID, a.ID, "POST", "/crons", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, code, out)
		}
	}
	path := "/crons/" + strconv.FormatInt(ops.ID, 10)
	if code, out := mailAs(t, m, testIsoID, a.ID, "PATCH", path, `{"enabled":false}`); code != http.StatusForbidden || !strings.Contains(out, "operator's") {
		t.Fatalf("changed the operator's: %d %s", code, out)
	}
	if code, out := mailAs(t, m, testIsoID, a.ID, "DELETE", path, ""); code != http.StatusForbidden {
		t.Fatalf("removed the operator's: %d %s", code, out)
	}
	if c := cronOf(t, m, a.ID, ops.ID); !c.Enabled {
		t.Fatal("the operator's cron was turned off")
	}

	path = "/crons/" + strconv.FormatInt(mine.ID, 10)
	if code, out := mailAs(t, m, testIsoID, a.ID, "PATCH", path, `{"schedule":"* * * * *"}`); code != http.StatusBadRequest {
		t.Fatalf("made its own too often: %d %s", code, out)
	}
	if code, out := mailAs(t, m, testIsoID, a.ID, "PATCH", path, `{"schedule":"30 9 * * 1-5","enabled":false}`); code != http.StatusOK {
		t.Fatalf("change: %d %s", code, out)
	}
	if c := cronOf(t, m, a.ID, mine.ID); c.Schedule != "30 9 * * 1-5" || c.Enabled {
		t.Fatalf("changed to %+v", c)
	}
	if code, out := mailAs(t, m, testIsoID, a.ID, "DELETE", path, ""); code != http.StatusOK {
		t.Fatalf("remove: %d %s", code, out)
	}

	// As many as it may, and no more.
	for i := range cronproxy.MaxCrons {
		if code, out := mailAs(t, m, testIsoID, a.ID, "POST", "/crons", `{"schedule":"@hourly","prompt":"n`+strconv.Itoa(i)+`"}`); code != http.StatusCreated {
			t.Fatalf("cron %d: %d %s", i, code, out)
		}
	}
	if code, out := mailAs(t, m, testIsoID, a.ID, "POST", "/crons", `{"schedule":"@hourly","prompt":"one more"}`); code != http.StatusTooManyRequests {
		t.Fatalf("one too many: %d %s", code, out)
	}
}

// A cron the agent set says so as it comes.
func TestACronOfItsOwnSaysTheAgentSetIt(t *testing.T) {
	ops := cronLetter(store.Cron{ID: 1, Schedule: "@daily", Prompt: "p"})
	own := cronLetter(store.Cron{ID: 2, Schedule: "@daily", Prompt: "p", Self: true})
	if !strings.Contains(ops, "Your operator set") || !strings.Contains(own, "You set this message yourself") {
		t.Fatalf("%q\n%q", ops, own)
	}
	if !strings.HasPrefix(own, `[gero cron #2, on schedule "@daily". `) || !strings.HasSuffix(own, "]\n\np") {
		t.Fatalf("%q reads unlike a cron", own)
	}
}

func TestTooOften(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)
	for spec, want := range map[string]bool{
		"@every 4m": true, "@every 5m": false, "*/5 * * * *": false, "*/4 * * * *": true,
		"0,2 * * * *": true, "58 * * * *": false, "@hourly": false, "59 23 * * *": false,
	} {
		s, err := cron.Parse(spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := tooOften(s, now); got != want {
			t.Errorf("%s: %v", spec, got)
		}
	}
}
