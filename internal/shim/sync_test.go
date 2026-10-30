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

func TestSyncPrunesWhatASkillNoLongerHas(t *testing.T) {
	dir, err := os.MkdirTemp("", "gero-sync")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	skills := filepath.Join(dir, "home", ".claude", "skills")
	file := func(p string) string { return filepath.Join(skills, p) }
	for _, p := range []string{"gero-skill-pdf/SKILL.md", "gero-skill-pdf/old.md", "gero-skill-pdf/scripts/gone.sh",
		"gero-skill-xlsx/SKILL.md", "gero-git/SKILL.md", "mine/SKILL.md"} {
		os.MkdirAll(filepath.Dir(file(p)), 0o755)
		os.WriteFile(file(p), []byte("x"), 0o644)
	}
	req := shimproto.SyncRequest{
		Files: []shimproto.SyncFile{
			{Path: file("gero-skill-pdf/SKILL.md"), Mode: 0o644, Agent: true, Content: "pdf"},
			{Path: file("gero-skill-pdf/logo.png"), Mode: 0o644, Agent: true, Content: "iVBORw0K", Base64: true},
		},
		Prune: []string{file("gero-skill-")},
	}
	if _, err := Sync(req, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		"gero-skill-pdf/SKILL.md": true, "gero-skill-pdf/logo.png": true,
		"gero-skill-pdf/old.md": false, "gero-skill-pdf/scripts": false, "gero-skill-xlsx": false,
		"gero-git/SKILL.md": true, "mine/SKILL.md": true,
	} {
		if _, err := os.Lstat(file(p)); (err == nil) != want {
			t.Errorf("%s: there %v, want %v", p, err == nil, want)
		}
	}
	if b, _ := os.ReadFile(file("gero-skill-pdf/logo.png")); string(b) != "\x89PNG\r\n" {
		t.Errorf("logo.png = %q, want it decoded", b)
	}
	if _, err := Sync(shimproto.SyncRequest{Prune: []string{file("skill-")}}, os.Getuid(), os.Getgid()); err == nil {
		t.Error("a prune of names not starting with gero was accepted")
	}
}
