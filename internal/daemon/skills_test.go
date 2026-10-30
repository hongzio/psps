package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/skillproxy"
	"github.com/hongzio/gero/internal/shimproto"
)

func TestASkillIsKeptOnTheMacAndWrittenWhereTheCLIsLook(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	t.Cleanup(stop)
	kept := filepath.Join(t.TempDir(), "skills")
	m.SetProxies(proxy.NewRegistry(skillproxy.Service{Dir: kept, Protected: []string{}}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	ctx := context.Background()

	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: pdf\ndescription: Read PDFs.\n---\n\nUse pdftotext.\n"), 0o644)
	os.WriteFile(filepath.Join(src, "logo.png"), []byte("\x89PNG\x00"), 0o644)
	if _, err := m.PutEnv(ctx, "skill", "PDF", proxy.Patch{Set: map[string]any{"path": src}}, true); err == nil {
		t.Fatal("a name the CLIs refuse was taken")
	}
	env, err := m.PutEnv(ctx, "skill", "pdf", proxy.Patch{Set: map[string]any{"path": src}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env.Config), `"description":"Read PDFs."`) {
		t.Fatalf("stored %s", env.Config)
	}
	before := m.serviceWorkspace(testIsoID).signature()
	if _, err := m.Expose(ctx, "dev", "skill", api.Expose{Envs: []string{"pdf"}}); err != nil {
		t.Fatal(err)
	}
	// The CLIs list skills as they start, so agents get a new process.
	exposed := m.serviceWorkspace(testIsoID).signature()
	if exposed == before {
		t.Fatal("exposing a skill left agents' processes as they were")
	}
	os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: pdf\ndescription: Read PDFs, now better.\n---\n\nUse pdftotext.\n"), 0o644)
	if _, err := m.PutEnv(ctx, "skill", "pdf", proxy.Patch{Set: map[string]any{"path": src}}, false); err != nil {
		t.Fatal(err)
	}
	if m.serviceWorkspace(testIsoID).signature() == exposed {
		t.Fatal("a skill copied in again left agents' processes as they were")
	}
	// Offered to an agent there at once, as its CLI names a skill.
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := m.Commands(a.ID)
	if i := slices.IndexFunc(cs.Commands, func(c api.Command) bool { return c.Prefix+c.Name == "$pdf" }); i < 0 || cs.Commands[i].Description != "Read PDFs, now better." {
		t.Fatalf("commands %v", commandNames(cs))
	}

	req, _ := e.fc.lastSync()
	var written []string
	for _, f := range req.Files {
		written = append(written, f.Path)
	}
	for _, want := range []string{"/home/agent/.claude/skills/gero-skill-pdf/SKILL.md", "/home/agent/.codex/skills/gero-skill-pdf/logo.png"} {
		if !slices.Contains(written, want) {
			t.Errorf("%s not written: %v", want, written)
		}
	}
	if !slices.Contains(req.Prune, "/home/agent/.claude/skills/gero-skill-") {
		t.Errorf("prune %v", req.Prune)
	}

	// An isolation whose shim cannot prune or decode gets the text files.
	m.mu.Lock()
	m.versions[testIsoID] = "4"
	delete(m.synced, testIsoID)
	m.mu.Unlock()
	iso, _ := m.store.Isolation(testIsoID)
	m.syncLocked(ctx, iso)
	req, _ = e.fc.lastSync()
	if len(req.Prune) != 0 || slices.ContainsFunc(req.Files, func(f shimproto.SyncFile) bool { return f.Base64 }) {
		t.Fatalf("an old shim was sent %v and binary files", req.Prune)
	}

	// Removed, it goes from the Mac and, pruned, from the isolation.
	m.mu.Lock()
	m.versions[testIsoID] = shimproto.Version
	m.mu.Unlock()
	if err := m.DeleteEnv(ctx, "skill", "pdf", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(kept, "pdf")); !os.IsNotExist(err) {
		t.Fatal("the kept skill survived its env")
	}
	req, _ = e.fc.lastSync()
	if slices.ContainsFunc(req.Files, func(f shimproto.SyncFile) bool { return strings.Contains(f.Path, "gero-skill-") }) || len(req.Prune) != 2 {
		t.Fatalf("after removal: prune %v", req.Prune)
	}
}
