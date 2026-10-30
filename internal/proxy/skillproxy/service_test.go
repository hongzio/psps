package skillproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

const good = `---
name: pdf
description: Fill in and read PDF forms. Use when asked about a .pdf file.
allowed-tools: Bash(python3 *) Read
disable-model-invocation: false
metadata:
  version: "1.2"
---

# PDF

Run [the filler](scripts/fill.py); the fields are in [reference](reference.md#fields).
`

// skill writes a skill's files under a new folder and returns it.
func skill(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	for p, c := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, ".py") {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func service(t *testing.T) Service {
	return Service{Dir: filepath.Join(t.TempDir(), "skills"), Protected: []string{}}
}

func TestASkillIsCheckedThenKept(t *testing.T) {
	s := service(t)
	src := skill(t, map[string]string{"SKILL.md": good, "scripts/fill.py": "print(1)\n", "reference.md": "# fields\n",
		"logo.png": "\x89PNG\r\n\x00", ".DS_Store": "junk", ".git/HEAD": "ref"})
	env := map[string]any{"path": src}
	w, err := s.Normalize("pdf", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 0 || env["path"] != nil || env["description"] != "Fill in and read PDF forms. Use when asked about a .pdf file." || env["files"] != 4 {
		t.Fatalf("%v %v", env, w)
	}
	kept := filepath.Join(s.Dir, "pdf")
	if st, err := os.Stat(filepath.Join(kept, "scripts/fill.py")); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("the script was not kept executable: %v", err)
	}
	for _, p := range []string{".DS_Store", ".git"} {
		if _, err := os.Stat(filepath.Join(kept, p)); err == nil {
			t.Errorf("%s was copied", p)
		}
	}

	// Edited without a path, the kept one is checked again.
	env = map[string]any{}
	if _, err := s.Normalize("pdf", env); err != nil || env["files"] != 4 {
		t.Fatalf("%v %v", env, err)
	}
	// A broken one does not replace it.
	bad := skill(t, map[string]string{"SKILL.md": "# no frontmatter\n"})
	if _, err := s.Normalize("pdf", map[string]any{"path": bad}); err == nil {
		t.Fatal("a skill without frontmatter was taken")
	}
	if b, _ := os.ReadFile(filepath.Join(kept, "SKILL.md")); string(b) != good {
		t.Fatal("a failed add changed the kept skill")
	}

	// In the isolation: both CLIs, under gero-skill-, binary files intact.
	raw, _ := json.Marshal(map[string]any{})
	ws, err := s.Workspace(proxy.View{Enabled: true, Exposure: store.Exposure{Envs: []string{"pdf", "gone"}}, Envs: map[string]json.RawMessage{"pdf": raw}})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, f := range ws.Files {
		paths[f.Path] = true
		if strings.HasSuffix(f.Path, "logo.png") != f.Base64 {
			t.Errorf("%s: base64 %v", f.Path, f.Base64)
		}
	}
	for _, p := range []string{"/home/agent/.claude/skills/gero-skill-pdf/SKILL.md", "/home/agent/.codex/skills/gero-skill-pdf/scripts/fill.py"} {
		if !paths[p] {
			t.Errorf("%s is not written; have %v", p, paths)
		}
	}
	if len(ws.Files) != 8 {
		t.Errorf("%d files written, want 8", len(ws.Files))
	}

	if err := s.Deleted("pdf"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Fatal("the kept skill survived its env")
	}
}

func TestASkillThatEitherCLIWouldDropIsRefused(t *testing.T) {
	fm := func(front string) string { return "---\n" + front + "\n---\n\nDo it.\n" }
	for why, files := range map[string]map[string]string{
		"no SKILL.md":            {"README.md": "x"},
		"lowercase skill.md":     {"skill.md": fm("name: pdf\ndescription: x")},
		"no frontmatter":         {"SKILL.md": "# PDF\n"},
		"unclosed frontmatter":   {"SKILL.md": "---\nname: pdf\ndescription: x\n"},
		"bad YAML":               {"SKILL.md": fm("name: pdf\ndescription: use it: when asked")},
		"duplicate key":          {"SKILL.md": fm("name: pdf\nname: pdf\ndescription: x")},
		"a list as frontmatter":  {"SKILL.md": fm("- name")},
		"no name":                {"SKILL.md": fm("description: x")},
		"another name":           {"SKILL.md": fm("name: docx\ndescription: x")},
		"no description":         {"SKILL.md": fm("name: pdf")},
		"empty description":      {"SKILL.md": fm("name: pdf\ndescription: \"  \"")},
		"long description":       {"SKILL.md": fm("name: pdf\ndescription: " + strings.Repeat("x", 1025))},
		"description as a list":  {"SKILL.md": fm("name: pdf\ndescription: [a, b]")},
		"non-boolean flag":       {"SKILL.md": fm("name: pdf\ndescription: x\nuser-invocable: maybe")},
		"unknown context":        {"SKILL.md": fm("name: pdf\ndescription: x\ncontext: thread")},
		"metadata not a mapping": {"SKILL.md": fm("name: pdf\ndescription: x\nmetadata: v1")},
		"nested metadata":        {"SKILL.md": fm("name: pdf\ndescription: x\nmetadata:\n  a:\n    b: c")},
		"byte order mark":        {"SKILL.md": "\ufeff" + fm("name: pdf\ndescription: x")},
		"binary SKILL.md":        {"SKILL.md": "---\nname: pdf\x00\n---\n"},
		"a file too big":         {"SKILL.md": fm("name: pdf\ndescription: x"), "big.bin": strings.Repeat("x", maxFileBytes+1)},
	} {
		t.Run(why, func(t *testing.T) {
			if _, err := service(t).Normalize("pdf", map[string]any{"path": skill(t, files)}); err == nil {
				t.Errorf("taken")
			}
		})
	}
}

func TestALinkInASkillIsRefused(t *testing.T) {
	src := skill(t, map[string]string{"SKILL.md": "---\nname: pdf\ndescription: x\n---\nhi\n"})
	secret := filepath.Join(t.TempDir(), "id_rsa")
	os.WriteFile(secret, []byte("key"), 0o600)
	os.Symlink(secret, filepath.Join(src, "key"))
	_, err := service(t).Normalize("pdf", map[string]any{"path": src})
	if err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Fatalf("a link was taken: %v", err)
	}
}

func TestSkillNames(t *testing.T) {
	for name, ok := range map[string]bool{
		"pdf": true, "pdf-forms-2": true, "PDF": false, "pdf_forms": false, "-pdf": false, "pdf-": false,
		"pdf--x": false, "gero-git": false, "geronimo": false, "synced": false, "anthropic-skills": false,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false,
	} {
		if err := CheckName(name); (err == nil) != ok {
			t.Errorf("CheckName(%q) = %v", name, err)
		}
	}
}

func TestWhatACLIIgnoresIsAWarning(t *testing.T) {
	src := skill(t, map[string]string{"SKILL.md": "---\nname: pdf\ndescription: Read <b>PDFs</b>\ncolour: blue\n---\n\nSee [the guide](guide.md) and [the site](https://example.com).\n"})
	w, err := service(t).Normalize("pdf", map[string]any{"path": src})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(w, "\n")
	for _, want := range []string{`"colour"`, "XML tag", "guide.md"} {
		if !strings.Contains(all, want) {
			t.Errorf("no warning about %s in %q", want, all)
		}
	}
	if strings.Contains(all, "example.com") {
		t.Errorf("a web link was warned about: %q", all)
	}
}

func TestASkillIsNotTakenFromGerosOwnDirectories(t *testing.T) {
	s := service(t)
	config := filepath.Join(t.TempDir(), "config")
	s.Protected = []string{config}
	os.MkdirAll(config, 0o700)
	os.WriteFile(filepath.Join(config, "SKILL.md"), []byte("---\nname: pdf\ndescription: x\n---\n"), 0o644)
	if _, err := s.Normalize("pdf", map[string]any{"path": config}); err == nil {
		t.Fatal("gero's config folder was taken as a skill")
	}
	if _, err := s.Normalize("pdf", map[string]any{}); err == nil || !strings.Contains(err.Error(), "--path") {
		t.Fatalf("a skill not kept yet was added without a path: %v", err)
	}
}
