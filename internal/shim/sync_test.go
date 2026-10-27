package shim

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hongzio/gero/internal/shimproto"
)

func TestManagedPathsAreGerosOnly(t *testing.T) {
	for path, want := range map[string]bool{
		"/etc/gero/env.sh": true,
		"/home/agent/.claude/skills/gero-git/SKILL.md": true,
		"/home/agent/.claude/skills/gero-git":          true,
		"/home/agent/.bashrc":                          false,
		"/home/agent/.claude/skills/mine/SKILL.md":     false,
		"/etc/gero/../passwd":                          false, // not clean
		"etc/gero/env.sh":                              false, // not absolute
		"/home/agent/work":                             false,
	} {
		if got := Managed(path); got != want {
			t.Errorf("Managed(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestSyncWritesOnlyWhatChangedAndRemovesWhatIsGone(t *testing.T) {
	dir, err := os.MkdirTemp("", "gero-sync")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	uid, gid := os.Getuid(), os.Getgid()
	skill := filepath.Join(dir, "home", ".claude", "skills", "gero-git", "SKILL.md")
	env := filepath.Join(dir, "etc", "env.sh")

	req := shimproto.SyncRequest{Files: []shimproto.SyncFile{
		{Path: skill, Mode: 0o644, Agent: true, Content: "skill v1"},
		{Path: env, Mode: 0o600, Agent: true, Content: "export A=1\n"},
	}}
	res, err := Sync(req, uid, gid)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 2 {
		t.Fatalf("first sync wrote %v", res.Written)
	}
	if st, _ := os.Stat(env); st.Mode().Perm() != 0o600 {
		t.Fatalf("env.sh mode = %v", st.Mode().Perm())
	}

	// The same request again changes nothing.
	if res, err = Sync(req, uid, gid); err != nil || len(res.Written) != 0 {
		t.Fatalf("idempotent sync wrote %v (%v)", res.Written, err)
	}

	// The skill's service was turned off: its directory goes.
	req = shimproto.SyncRequest{
		Files:  []shimproto.SyncFile{{Path: env, Mode: 0o600, Agent: true, Content: "export A=2\n"}},
		Remove: []string{filepath.Dir(skill), filepath.Join(dir, "home", ".claude", "skills", "gero-pg")},
	}
	res, err = Sync(req, uid, gid)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 1 || len(res.Removed) != 1 {
		t.Fatalf("second sync: %+v", res)
	}
	if _, err := os.Stat(filepath.Dir(skill)); !os.IsNotExist(err) {
		t.Fatal("the skill directory survived its removal")
	}
	if b, _ := os.ReadFile(env); string(b) != "export A=2\n" {
		t.Fatalf("env.sh = %q", b)
	}
}

func TestSyncRefusesAPathItDoesNotManage(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(victim, []byte("mine"), 0o644)
	_, err := Sync(shimproto.SyncRequest{Remove: []string{victim}}, os.Getuid(), os.Getgid())
	if err == nil {
		t.Fatal("a removal outside gero's paths was accepted")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("the file was removed anyway")
	}
}
