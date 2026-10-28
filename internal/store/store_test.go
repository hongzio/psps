package store

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
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
	if err := s.PutIsolation(Isolation{Name: "dev", Container: "gero-dev", Image: "img", CreatedAt: now,
		Env: map[string]string{"K": "v"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAgent(Agent{ID: "a1", Name: "x", Kind: driver.Codex, Isolation: "dev", Workdir: "/w",
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
	s.PutIsolation(Isolation{Name: "dev", Container: "c", Image: "i", CreatedAt: now})
	s.CreateAgent(Agent{ID: "a1", Name: "x", Kind: driver.Claude, Isolation: "dev", Workdir: "/w", CreatedAt: now})
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
		if err := s.PutIsolation(Isolation{Name: n, Container: "gero-" + n, Image: "img", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	s.PutEnv(Env{Service: "datadog", Name: "prod", Config: []byte(`{"site":"datadoghq.com"}`)})
	s.PutEnv(Env{Service: "datadog", Name: "stage"})
	s.PutExposure(Exposure{Isolation: "dev", Service: "datadog", Envs: []string{"prod", "stage"}, Default: "prod"})
	s.PutExposure(Exposure{Isolation: "ci", Service: "datadog", Envs: []string{"stage"}, Default: "stage"})

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
		if err := s.PutIsolation(Isolation{Name: n, Container: "gero-" + n, Image: "i", CreatedAt: time.Now()}); err != nil {
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
	if err := s.PutIsolation(Isolation{Name: "dev", Container: "c", Image: "i", Env: map[string]string{"K": "secret-1234567890"}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIsolation(Isolation{Name: "dev", Container: "c", Image: "i", CreatedAt: time.Now()}); err != nil {
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

func TestRenamingAnIsolationMovesEverythingThatNamesIt(t *testing.T) {
	s := open(t)
	now := time.Now()
	for _, n := range []string{"dev", "other"} {
		if err := s.PutIsolation(Isolation{Name: n, Container: "gero-" + n, Image: "img", Network: "gero-" + n + "-net",
			CreatedAt: now, Env: map[string]string{"K": "v"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateAgent(Agent{ID: "a1", Name: "x", Kind: driver.Codex, Isolation: "dev", Workdir: "/w", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutExposure(Exposure{Isolation: "dev", Service: "git", Envs: []string{}}); err != nil {
		t.Fatal(err)
	}
	toks, err := s.EnsureTokens("dev", "git", []string{""})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutAccount(Account{Name: "work", Kind: "claude-token"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UseAccount("dev", driver.Claude, "work"); err != nil {
		t.Fatal(err)
	}

	if err := s.RenameIsolation("dev", "other"); !errors.Is(err, ErrExists) {
		t.Fatalf("renamed onto another isolation: %v", err)
	}
	if err := s.RenameIsolation("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renamed a missing isolation: %v", err)
	}
	if err := s.RenameIsolation("dev", "work"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Isolation("dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old name is still there: %v", err)
	}
	iso, err := s.Isolation("work")
	if err != nil {
		t.Fatal(err)
	}
	if iso.Base != "dev" || iso.BaseName() != "dev" || iso.Container != "gero-dev" || iso.Network != "gero-dev-net" || iso.Env["K"] != "v" {
		t.Fatalf("renamed isolation: %+v", iso)
	}
	if a, _ := s.Agent("a1"); a.Isolation != "work" {
		t.Fatalf("agent is in %q", a.Isolation)
	}
	if _, err := s.Exposure("work", "git"); err != nil {
		t.Fatalf("exposure: %v", err)
	}
	if got, _ := s.Tokens("work", "git"); got[""] != toks[""] || got[""] == "" {
		t.Fatalf("tokens %v, had %v: the agents' environment would no longer match", got, toks)
	}
	if used, _ := s.AccountsUsed("work"); used[driver.Claude] != "work" {
		t.Fatalf("accounts %v", used)
	}
	var broken int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&broken); err != nil || broken != 0 {
		t.Fatalf("foreign keys: %d broken (%v)", broken, err)
	}

	// Renamed again, it is still named after where it started.
	if err := s.RenameIsolation("work", "dev2"); err != nil {
		t.Fatal(err)
	}
	if iso, _ := s.Isolation("dev2"); iso.BaseName() != "dev" {
		t.Fatalf("base after a second rename: %q", iso.BaseName())
	}
}
