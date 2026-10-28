package store

import (
	"bytes"
	"database/sql"
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
