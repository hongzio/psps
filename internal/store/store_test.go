package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "gero.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCommitAssignsIDsAndAdvancesLastSrc(t *testing.T) {
	s := open(t)
	now := time.Now()
	if err := s.PutIsolation(Isolation{ID: "dev", Name: "dev", Container: "gero-dev", Image: "img", CreatedAt: now,
		Env: map[string]string{"K": "v"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAgent(Agent{ID: "a1", Name: "x", Kind: driver.Codex, IsolationID: "dev", Workdir: "/w",
		Options: driver.Options{Effort: "low"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Commit("a1", 7, []event.Event{
		{Agent: "a1", Src: 7, Kind: event.KindUser, Text: "hi"},
		{Agent: "a1", Src: 7, Kind: event.KindDelta, Ephemeral: true},
		{Agent: "a1", Src: 7, Kind: event.KindStatus, Status: event.StatusWorking},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("committed = %+v", got)
	}
	// An older src never moves the read position backwards.
	s.Commit("a1", 3, nil)
	a, _ := s.Agent("a1")
	if a.LastSrc != 7 || a.Options.Effort != "low" {
		t.Fatalf("agent = %+v", a)
	}
	evs, _ := s.Events("a1", 1, 0)
	if len(evs) != 1 || evs[0].Status != event.StatusWorking {
		t.Fatalf("events after 1 = %+v", evs)
	}
	iso, _ := s.Isolation("dev")
	if iso.Env["K"] != "v" {
		t.Fatalf("isolation env = %v", iso.Env)
	}
	if err := s.DeleteIsolation("dev"); err == nil {
		t.Fatal("deleted an isolation that still has agents")
	}
}

func TestPendingRoundTrip(t *testing.T) {
	s := open(t)
	now := time.Now()
	s.PutIsolation(Isolation{ID: "dev", Name: "dev", Container: "c", Image: "i", CreatedAt: now})
	s.CreateAgent(Agent{ID: "a1", Name: "x", Kind: driver.Claude, IsolationID: "dev", Workdir: "/w", CreatedAt: now})
	id1, _ := s.AddPending("a1", "one", 3)
	s.AddPending("a1", "two", 4)
	s.DeletePending(id1)
	p, err := s.Pending("a1")
	if err != nil || len(p) != 1 || p[0].Text != "two" || p[0].After != 4 {
		t.Fatalf("pending = %+v, %v", p, err)
	}
}

func TestMigrationAddsColumnsToAnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The isolations table as it shipped before the fence.
	db.Exec(`CREATE TABLE isolations (name TEXT PRIMARY KEY, container TEXT NOT NULL, image TEXT NOT NULL,
		cpus INTEGER NOT NULL DEFAULT 0, memory TEXT NOT NULL DEFAULT '', env TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL)`)
	db.Exec(`INSERT INTO isolations (name, container, image, created_at) VALUES ('dev', 'gero-dev', 'img', '2026-01-01T00:00:00Z')`)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	iso, err := s.Isolation("dev")
	if err != nil || iso.Fence != "on" || iso.Network != "" {
		t.Fatalf("migrated isolation = %+v, %v", iso, err)
	}
	iso.Network = "gero-dev-net"
	s.PutIsolation(iso)
	if got, _ := s.Isolation("dev"); got.Network != "gero-dev-net" {
		t.Fatalf("network not stored: %+v", got)
	}
}

func TestEnvsExposuresAndTokens(t *testing.T) {
	s := open(t)
	for _, n := range []string{"dev", "ci"} {
		if err := s.PutIsolation(Isolation{ID: n, Name: n, Container: "gero-" + n, Image: "img", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	s.PutEnv(Env{Service: "datadog", Name: "prod", Config: []byte(`{"site":"datadoghq.com"}`)})
	s.PutEnv(Env{Service: "datadog", Name: "stage"})
	s.PutExposure(Exposure{IsolationID: "dev", Service: "datadog", Envs: []string{"prod", "stage"}, Default: "prod"})
	s.PutExposure(Exposure{IsolationID: "ci", Service: "datadog", Envs: []string{"stage"}, Default: "stage"})

	toks, err := s.EnsureTokens("dev", "datadog", []string{"prod", "stage"})
	if err != nil || len(toks) != 2 || toks["prod"] == toks["stage"] {
		t.Fatalf("tokens = %v %v", toks, err)
	}
	again, _ := s.EnsureTokens("dev", "datadog", []string{"prod"})
	if again["prod"] != toks["prod"] || len(again) != 1 {
		t.Fatalf("a kept token changed or a dropped one stayed: %v", again)
	}

	// An env still exposed cannot be deleted without force...
	var exposed *ErrExposed
	if err := s.DeleteEnv("datadog", "stage", false); !errors.As(err, &exposed) || len(exposed.Isolations) != 2 {
		t.Fatalf("delete of an exposed env: %v", err)
	}
	// ...and with force it leaves every exposure consistent.
	s.EnsureTokens("dev", "datadog", []string{"prod", "stage"})
	if err := s.DeleteEnv("datadog", "stage", true); err != nil {
		t.Fatal(err)
	}
	ci, _ := s.Exposure("ci", "datadog")
	if len(ci.Envs) != 0 || ci.Default != "" {
		t.Fatalf("ci after the forced delete: %+v", ci)
	}
	if toks, _ := s.Tokens("dev", "datadog"); len(toks) != 1 {
		t.Fatalf("the deleted env's token survived: %v", toks)
	}

	// Deleting an isolation takes its exposures and tokens with it.
	if err := s.DeleteIsolation("dev"); err != nil {
		t.Fatal(err)
	}
	if xs, _ := s.Exposures("dev"); len(xs) != 0 {
		t.Fatalf("exposures outlived the isolation: %v", xs)
	}
	if toks, _ := s.Tokens("dev", "datadog"); len(toks) != 0 {
		t.Fatalf("tokens outlived the isolation: %v", toks)
	}
}

func TestAccountUsesGoWithTheirIsolation(t *testing.T) {
	s := open(t)
	for _, n := range []string{"dev", "lab"} {
		if err := s.PutIsolation(Isolation{ID: n, Name: n, Container: "gero-" + n, Image: "i", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutAccount(Account{Name: "claude", Kind: "claude-token"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UseAccount("dev", driver.Claude, "nope"); err == nil {
		t.Fatal("used an account that does not exist")
	}
	for _, n := range []string{"dev", "lab"} {
		if err := s.UseAccount(n, driver.Claude, "claude"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteIsolation("lab"); err != nil {
		t.Fatal(err)
	}
	if users, _ := s.AccountUsers("claude"); len(users) != 1 || users[0] != "dev" {
		t.Fatalf("%v", users)
	}
	if err := s.UseAccount("dev", driver.Claude, ""); err != nil {
		t.Fatal(err)
	}
	if used, _ := s.AccountsUsed("dev"); len(used) != 0 {
		t.Fatalf("%v", used)
	}
}

func TestTheStoreIsTheOwnersAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gero.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// What Vacuum is for: a secret deleted from the store is gone from its
	// files.
	if err := s.PutIsolation(Isolation{ID: "dev", Name: "dev", Container: "c", Image: "i", Env: map[string]string{"K": "secret-1234567890"}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIsolation(Isolation{ID: "dev", Name: "dev", Container: "c", Image: "i", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Vacuum(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal"} {
		b, _ := os.ReadFile(f)
		if bytes.Contains(b, []byte("secret-1234567890")) {
			t.Errorf("%s still holds the deleted secret", filepath.Base(f))
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("store is %v", fi.Mode().Perm())
	}
}

// A rename is the name alone: everything else knows the isolation by ID.
func TestRenamingAnIsolationChangesItsNameAlone(t *testing.T) {
	s := open(t)
	now := time.Now()
	for _, n := range []string{"dev", "other"} {
		if err := s.PutIsolation(Isolation{ID: "id-" + n, Name: n, Container: "gero-id-" + n, Image: "img", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	// Everything that belongs to an isolation points at its ID, foreign
	// keys included.
	if err := s.CreateAgent(Agent{ID: "a1", Name: "x", Kind: driver.Codex, IsolationID: "id-dev", Workdir: "/w", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutExposure(Exposure{IsolationID: "id-dev", Service: "git", Envs: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureTokens("id-dev", "git", []string{""}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAccount(Account{Name: "claude", Kind: "claude-token"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UseAccount("id-dev", driver.Claude, "claude"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameIsolation("id-dev", "other"); !errors.Is(err, ErrExists) {
		t.Fatalf("renamed onto another isolation's name: %v", err)
	}
	if err := s.RenameIsolation("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renamed a missing isolation: %v", err)
	}
	if err := s.RenameIsolation("id-dev", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IsolationNamed("dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old name still finds it: %v", err)
	}
	iso, err := s.IsolationNamed("work")
	if err != nil || iso.ID != "id-dev" || iso.Container != "gero-id-dev" {
		t.Fatalf("renamed: %+v, %v", iso, err)
	}
	if a, _ := s.Agent("a1"); a.IsolationID != "id-dev" {
		t.Fatalf("agent is in %q", a.IsolationID)
	}
	if err := s.PutIsolation(Isolation{ID: "id-new", Name: "work", Container: "c", Image: "img", CreatedAt: now}); err == nil {
		t.Fatal("two isolations took one name")
	}
}

// Isolations from before IDs: the column that was the name becomes the ID,
// so whatever pointed at one still does, and a renamed one's ID is the name
// its container and volumes were made under.
func TestIsolationsFromBeforeIDsKeepWhatPointsAtThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE isolations (name TEXT PRIMARY KEY, container TEXT NOT NULL, image TEXT NOT NULL,
			cpus INTEGER NOT NULL DEFAULT 0, memory TEXT NOT NULL DEFAULT '', env TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL,
			network TEXT NOT NULL DEFAULT '', fence TEXT NOT NULL DEFAULT 'on', base TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE agents (id TEXT PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL,
			isolation TEXT NOT NULL REFERENCES isolations(name), workdir TEXT NOT NULL, repo TEXT NOT NULL DEFAULT '',
			options TEXT NOT NULL DEFAULT '{}', session_id TEXT NOT NULL DEFAULT '', last_src INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL, archived_at TEXT)`,
		`CREATE TABLE exposures (isolation TEXT NOT NULL REFERENCES isolations(name) ON DELETE CASCADE, service TEXT NOT NULL,
			envs TEXT NOT NULL DEFAULT '[]', default_env TEXT NOT NULL DEFAULT '', settings TEXT NOT NULL DEFAULT '{}',
			PRIMARY KEY (isolation, service))`,
		`CREATE TABLE tokens (isolation TEXT NOT NULL REFERENCES isolations(name) ON DELETE CASCADE, service TEXT NOT NULL,
			env TEXT NOT NULL, token TEXT NOT NULL, PRIMARY KEY (isolation, service, env))`,
		`CREATE TABLE accounts (name TEXT PRIMARY KEY, kind TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE account_links (isolation TEXT NOT NULL REFERENCES isolations(name) ON DELETE CASCADE, tool TEXT NOT NULL,
			account TEXT NOT NULL REFERENCES accounts(name), PRIMARY KEY (isolation, tool))`,
		// work was dev until renamed; the dev made after it is named
		// dev-3f2a underneath; ci never moved.
		`INSERT INTO isolations (name, container, image, created_at, network, base) VALUES
			('work', 'gero-dev', 'img', '2026-01-01T00:00:00Z', 'gero-dev-net', 'dev'),
			('dev', 'gero-dev-3f2a', 'img', '2026-01-02T00:00:00Z', 'gero-dev-3f2a-net', 'dev-3f2a'),
			('ci', 'gero-ci', 'img', '2026-01-03T00:00:00Z', 'gero-ci-net', '')`,
		`INSERT INTO agents (id, name, kind, isolation, workdir, created_at) VALUES
			('a1', 'x', 'codex', 'work', '/w', '2026-01-01T00:00:00Z'), ('a2', 'y', 'claude', 'dev', '/w', '2026-01-02T00:00:00Z')`,
		`INSERT INTO exposures (isolation, service) VALUES ('work', 'git')`,
		`INSERT INTO tokens (isolation, service, env, token) VALUES ('ci', 'git', '', 'tok')`,
		`INSERT INTO accounts (name, kind, created_at) VALUES ('claude', 'claude-token', '2026-01-01T00:00:00Z')`,
		`INSERT INTO account_links (isolation, tool, account) VALUES ('dev', 'claude', 'claude')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for id, name := range map[string]string{"dev": "work", "dev-3f2a": "dev", "ci": "ci"} {
		iso, err := s.Isolation(id)
		if err != nil || iso.Name != name {
			t.Fatalf("isolation %s = %+v, %v; want it named %s", id, iso, err, name)
		}
		if iso.Container != "gero-"+id {
			t.Fatalf("isolation %s's container is %s", id, iso.Container)
		}
	}
	if a, _ := s.Agent("a1"); a.IsolationID != "dev" {
		t.Fatalf("a1 is in %q", a.IsolationID)
	}
	if a, _ := s.Agent("a2"); a.IsolationID != "dev-3f2a" {
		t.Fatalf("a2 is in %q", a.IsolationID)
	}
	if _, err := s.Exposure("dev", "git"); err != nil {
		t.Fatalf("work's exposure: %v", err)
	}
	if toks, _ := s.Tokens("ci", "git"); toks[""] != "tok" {
		t.Fatalf("ci's tokens: %v", toks)
	}
	if used, _ := s.AccountsUsed("dev-3f2a"); used[driver.Claude] != "claude" {
		t.Fatalf("the new dev's accounts: %v", used)
	}
	var broken int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&broken); err != nil || broken != 0 {
		t.Fatalf("foreign keys: %d broken (%v)", broken, err)
	}
	if have, _ := hasColumn(s.db, "isolations", "base"); have {
		t.Fatal("base is still a column")
	}
	var agents string
	s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'agents'`).Scan(&agents)
	if !strings.Contains(agents, "REFERENCES isolations(id)") {
		t.Fatalf("agents still point at the name: %s", agents)
	}
	if err := s.RenameIsolation("dev", "dev"); !errors.Is(err, ErrExists) {
		t.Fatalf("a name already taken: %v", err)
	}

	// Opened again, there is nothing left to migrate.
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if iso, _ := s.IsolationNamed("work"); iso.ID != "dev" {
		t.Fatalf("after reopening: %+v", iso)
	}
}

// An agent's own ID is unique among all agents, as anyone's own ID or as
// anyone's key, archived agents included; giving it back frees it.
func TestAnAgentsOwnIDIsNobodyElses(t *testing.T) {
	s := open(t)
	now := time.Now()
	if err := s.PutIsolation(Isolation{ID: "i1", Name: "dev", Container: "gero-i1", Image: "img", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	agent := func(id, handle string) Agent {
		return Agent{ID: id, Handle: handle, Name: id, Kind: driver.Codex, IsolationID: "i1", Workdir: "/w/" + id, CreatedAt: now}
	}
	if err := s.CreateAgent(agent("a1", "login")); err != nil {
		t.Fatal(err)
	}
	s.SetArchived("a1", &now)
	if err := s.CreateAgent(agent("a2", "")); err != nil {
		t.Fatal(err)
	}
	for _, a := range []Agent{agent("a3", "login"), agent("a3", "a1"), agent("login", "")} {
		if err := s.CreateAgent(a); !errors.Is(err, ErrExists) {
			t.Errorf("created %s (own ID %q): %v", a.ID, a.Handle, err)
		}
	}
	h := func(v string) *string { return &v }
	for _, taken := range []string{"login", "a1"} {
		if err := s.RenameAgent("a2", nil, h(taken)); !errors.Is(err, ErrExists) {
			t.Errorf("a2 took %q: %v", taken, err)
		}
	}
	if err := s.RenameAgent("a1", h("renamed"), h("login")); err != nil {
		t.Fatalf("an agent keeping its own ID: %v", err)
	}
	if a, _ := s.Agent("a1"); a.Name != "renamed" || a.Handle != "login" {
		t.Fatalf("a1 is %+v", a)
	}
	if err := s.RenameAgent("a1", nil, h("")); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameAgent("a2", nil, h("login")); err != nil {
		t.Fatalf("a freed ID: %v", err)
	}
	if a, err := s.AgentByHandle("login"); err != nil || a.ID != "a2" || a.Name != "a2" {
		t.Fatalf("login is %+v, %v", a, err)
	}
	if err := s.RenameAgent("nope", nil, h("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renamed nobody: %v", err)
	}
}

func TestMailWaitsInItsInboxAndLeavesItOnce(t *testing.T) {
	s := open(t)
	now := time.Now()
	s.PutIsolation(Isolation{ID: "i1", Name: "dev", Container: "gero-i1", Image: "img", CreatedAt: now})
	for _, id := range []string{"a1", "b1"} {
		if err := s.CreateAgent(Agent{ID: id, Name: id, Kind: driver.Codex, IsolationID: "i1", Workdir: "/w", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	var ids []int64
	for i, body := range []string{"one", "two", "three"} {
		m, waiting, err := s.AddMail(Mail{To: "b1", From: "a1", FromID: "alice", FromIsolation: "i1", Body: body}, 3)
		if err != nil || waiting != i+1 {
			t.Fatalf("%s: %d waiting, %v", body, waiting, err)
		}
		ids = append(ids, m.ID)
	}
	if _, waiting, err := s.AddMail(Mail{To: "b1", From: "a1", Body: "four"}, 3); !errors.Is(err, ErrInboxFull) || waiting != 3 {
		t.Fatalf("a full inbox took more: %d, %v", waiting, err)
	}

	next, err := s.NextMail("b1")
	if err != nil || next.ID != ids[0] || next.Body != "one" || next.FromID != "alice" {
		t.Fatalf("next %+v %v", next, err)
	}
	p, err := s.DeliverMail(next.ID, "letter one", 9)
	if err != nil {
		t.Fatal(err)
	}
	// Out of the inbox and into the outbox, together.
	if pend, _ := s.Pending("b1"); len(pend) != 1 || pend[0] != p || p.Text != "letter one" || p.After != 9 {
		t.Fatalf("outbox %+v, delivered %+v", pend, p)
	}
	if _, err := s.DeliverMail(next.ID, "letter one", 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delivered twice: %v", err)
	}
	if n, _ := s.InboxCount("b1"); n != 2 {
		t.Fatalf("%d waiting after a delivery", n)
	}
	if pend, _ := s.Pending("b1"); len(pend) != 1 {
		t.Fatalf("a second delivery added %+v", pend)
	}

	if err := s.DiscardMail("a1", ids[1]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted from another agent's inbox: %v", err)
	}
	if err := s.DiscardMail("b1", ids[1]); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.Inbox("b1"); len(in) != 1 || in[0].Body != "three" {
		t.Fatalf("inbox %+v", in)
	}
	if n, _ := s.InboxCounts(); n["b1"] != 1 || len(n) != 1 {
		t.Fatalf("counts %v", n)
	}
	if n, err := s.ClearInbox("b1"); n != 1 || err != nil {
		t.Fatalf("cleared %d, %v", n, err)
	}
	if n, _ := s.InboxCounts(); len(n) != 0 {
		t.Fatalf("counts after clearing %v", n)
	}
	// Delivered or discarded, mail stays as the two agents' history.
	hist, _ := s.MailOf("b1", 10)
	var states []string
	for _, m := range hist {
		state := "waiting"
		switch {
		case m.DeliveredAt != nil:
			state = "delivered"
		case m.DiscardedAt != nil:
			state = "discarded"
		}
		states = append(states, m.Body+":"+state)
	}
	if got := strings.Join(states, " "); got != "three:discarded two:discarded one:delivered" {
		t.Fatalf("b1's history: %s", got)
	}
	if sent, _ := s.MailOf("a1", 10); len(sent) != 3 {
		t.Fatalf("a1 wrote %d", len(sent))
	}
	if last, _ := s.MailOf("b1", 1); len(last) != 1 || last[0].Body != "three" {
		t.Fatalf("limit: %+v", last)
	}

	// A deleted agent's mail goes with it; what it wrote stays.
	s.AddMail(Mail{To: "b1", From: "a1", Body: "for b"}, 3)
	s.AddMail(Mail{To: "a1", From: "b1", Body: "from b"}, 3)
	if err := s.DeleteAgent("b1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.InboxCounts(); n["b1"] != 0 || n["a1"] != 1 {
		t.Fatalf("counts after delete %v", n)
	}
	// Mail b1 was sent went with it, from its sender's history too.
	if hist, _ := s.MailOf("a1", 10); len(hist) != 1 || hist[0].Body != "from b" {
		t.Fatalf("a1's history after b1 went: %+v", hist)
	}
}

func TestAnAgentsMailTokenIsItsAloneAndShownNowhere(t *testing.T) {
	s := open(t)
	now := time.Now()
	s.PutIsolation(Isolation{ID: "i1", Name: "dev", Container: "gero-i1", Image: "img", CreatedAt: now})
	s.CreateAgent(Agent{ID: "a1", Name: "a", Kind: driver.Codex, IsolationID: "i1", Workdir: "/w", CreatedAt: now})
	s.CreateAgent(Agent{ID: "b1", Name: "b", Kind: driver.Codex, IsolationID: "i1", Workdir: "/w", CreatedAt: now})
	ta, err := s.MailToken("a1")
	if err != nil || len(ta) < 40 {
		t.Fatalf("%q %v", ta, err)
	}
	if again, _ := s.MailToken("a1"); again != ta {
		t.Fatal("the token changed")
	}
	if tb, _ := s.MailToken("b1"); tb == ta {
		t.Fatal("two agents share a token")
	}
	if a, err := s.AgentByMailToken(ta); err != nil || a.ID != "a1" {
		t.Fatalf("%+v %v", a, err)
	}
	for _, tok := range []string{"", "nope"} {
		if _, err := s.AgentByMailToken(tok); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q: %v", tok, err)
		}
	}
	if _, err := s.MailToken("nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	a, _ := s.Agent("a1")
	if b, _ := json.Marshal(a); bytes.Contains(b, []byte(ta)) {
		t.Fatalf("shown: %s", b)
	}
}

// Mail from before the history keeps waiting where it waited.
func TestMailFromBeforeTheHistoryStillWaits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gero.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.PutIsolation(Isolation{ID: "i1", Name: "dev", Container: "gero-i1", Image: "img", CreatedAt: now})
	s.CreateAgent(Agent{ID: "b1", Name: "b", Kind: driver.Codex, IsolationID: "i1", Workdir: "/w", CreatedAt: now})
	// The mail table as it first shipped: a delivered mail left it.
	for _, q := range []string{`DROP TABLE mail`, `CREATE TABLE mail (id INTEGER PRIMARY KEY AUTOINCREMENT, agent TEXT NOT NULL REFERENCES agents(id),
		sender TEXT NOT NULL, sender_id TEXT NOT NULL, sender_iso TEXT NOT NULL, body TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`INSERT INTO mail (agent, sender, sender_id, sender_iso, body, created_at) VALUES ('b1', 'a1', 'alice', 'i1', 'old', '2026-10-29T00:00:00Z')`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if in, err := s.Inbox("b1"); err != nil || len(in) != 1 || in[0].Body != "old" || !in[0].Waiting() {
		t.Fatalf("%+v %v", in, err)
	}
}
