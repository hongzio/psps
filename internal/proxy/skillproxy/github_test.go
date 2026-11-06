package skillproxy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestASourceIsReadAsGitHubSpellsIt(t *testing.T) {
	for in, want := range map[string]Spec{
		"acme/skills":                                                  {Repo: "acme/skills"},
		"acme/skills.git":                                              {Repo: "acme/skills"},
		"acme/skills@v1.2.0":                                           {Repo: "acme/skills", Ref: "v1.2.0"},
		"acme/skills/skills/pdf":                                       {Repo: "acme/skills", Path: "skills/pdf", Exact: true},
		"acme/skills/skills/pdf/SKILL.md":                              {Repo: "acme/skills", Path: "skills/pdf", Exact: true},
		"acme/skills/skills/pdf@main":                                  {Repo: "acme/skills", Ref: "main", Path: "skills/pdf", Exact: true},
		"https://github.com/acme/skills":                               {Repo: "acme/skills"},
		"github.com/acme/skills/":                                      {Repo: "acme/skills"},
		"https://github.com/acme/skills#readme":                        {Repo: "acme/skills"},
		"https://github.com/acme/skills/tree/feature/x/skills/pdf":     {Repo: "acme/skills", Exact: true, refPath: []string{"feature", "x", "skills", "pdf"}},
		"https://github.com/acme/skills/blob/main/skills/pdf/SKILL.md": {Repo: "acme/skills", Exact: true, blob: true, refPath: []string{"main", "skills", "pdf", "SKILL.md"}},
	} {
		got, err := ParseSpec(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got.Repo != want.Repo || got.Ref != want.Ref || got.Path != want.Path || got.Exact != want.Exact || got.blob != want.blob || !slices.Equal(got.refPath, want.refPath) {
			t.Errorf("%s: got %+v, want %+v", in, got, want)
		}
	}
	for _, in := range []string{"acme", "git@github.com:acme/skills.git", "https://gitlab.com/acme/skills", "acme/skills/../x", "-acme/skills", "acme/skills@", "ftp://github.com/acme/skills", "https://github.com/acme/skills/tree"} {
		if _, err := ParseSpec(in); err == nil {
			t.Errorf("%s was taken", in)
		}
	}
}

// fakeGitHub is a public repository, acme/skills, as GitHub serves it.
type fakeGitHub struct {
	mu      sync.Mutex
	def     string
	refs    map[string]string
	commits map[string]map[string]fakeFile
	calls   int
}

type fakeFile struct {
	body string
	exec bool
	link string
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, Service) {
	f := &fakeGitHub{def: "main", refs: map[string]string{}, commits: map[string]map[string]fakeFile{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, Service{Dir: filepath.Join(t.TempDir(), "skills"), Protected: []string{}, API: srv.URL, Raw: srv.URL + "/raw", Client: srv.Client()}
}

// push makes files the repository's state on ref, and returns the commit.
func (f *fakeGitHub) push(ref string, files map[string]fakeFile) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := sha1.New()
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	fmt.Fprint(h, ref, len(f.commits))
	for _, k := range keys {
		fmt.Fprint(h, k, files[k])
	}
	sha := hex.EncodeToString(h.Sum(nil))
	f.commits[sha] = files
	f.refs[ref] = sha
	return sha
}

// treeOf is the tree of dir at a commit: a hash of what is under it.
func treeOf(files map[string]fakeFile, dir string) string {
	h := sha1.New()
	keys := make([]string, 0, len(files))
	for k := range files {
		if dir == "" || strings.HasPrefix(k, dir+"/") {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprint(h, strings.TrimPrefix(k, dir+"/"), files[k])
	}
	return "t" + hex.EncodeToString(h.Sum(nil))
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	p := r.URL.Path
	commit := func(ref string) (string, bool) {
		if sha, ok := f.refs[ref]; ok {
			return sha, true
		}
		for sha := range f.commits {
			if len(ref) >= 7 && strings.HasPrefix(sha, ref) {
				return sha, true
			}
		}
		return "", false
	}
	js := func(v any) { json.NewEncoder(w).Encode(v) }
	switch {
	case p == "/repos/acme/skills":
		js(map[string]string{"default_branch": f.def})
	case strings.HasPrefix(p, "/repos/acme/skills/commits/"):
		sha, ok := commit(strings.TrimPrefix(p, "/repos/acme/skills/commits/"))
		if !ok {
			http.Error(w, "{}", 422)
			return
		}
		js(map[string]any{"sha": sha, "commit": map[string]any{"tree": map[string]string{"sha": treeOf(f.commits[sha], "")}}})
	case strings.HasPrefix(p, "/repos/acme/skills/contents"):
		files := f.commits[r.URL.Query().Get("ref")]
		parent := strings.Trim(strings.TrimPrefix(p, "/repos/acme/skills/contents"), "/")
		seen := map[string]bool{}
		var out []map[string]string
		for k := range files {
			rest, ok := strings.CutPrefix(k, parent+"/")
			if parent == "" {
				rest, ok = k, true
			}
			if dir, _, isDir := strings.Cut(rest, "/"); ok && isDir && !seen[dir] {
				seen[dir] = true
				out = append(out, map[string]string{"name": dir, "type": "dir", "sha": treeOf(files, path.Join(parent, dir))})
			}
		}
		if out == nil {
			http.NotFound(w, r)
			return
		}
		js(out)
	case strings.HasPrefix(p, "/repos/acme/skills/git/trees/"):
		files := f.commits[strings.TrimPrefix(p, "/repos/acme/skills/git/trees/")]
		var tree []map[string]string
		for k := range files {
			tree = append(tree, map[string]string{"path": k, "type": "blob"})
		}
		js(map[string]any{"tree": tree})
	case strings.HasPrefix(p, "/raw/acme/skills/"):
		sha, file, _ := strings.Cut(strings.TrimPrefix(p, "/raw/acme/skills/"), "/")
		ff, ok := f.commits[sha][file]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(ff.body))
	case strings.HasPrefix(p, "/repos/acme/skills/tarball/"):
		sha := strings.TrimPrefix(p, "/repos/acme/skills/tarball/")
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		top := "acme-skills-" + sha[:7] + "/"
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": sha}})
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top, Mode: 0o755})
		keys := make([]string, 0)
		for k := range f.commits[sha] {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			ff := f.commits[sha][k]
			if ff.link != "" {
				tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: top + k, Linkname: ff.link, Mode: 0o777})
				continue
			}
			mode := int64(0o644)
			if ff.exec {
				mode = 0o755
			}
			tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + k, Mode: mode, Size: int64(len(ff.body))})
			tw.Write([]byte(ff.body))
		}
		tw.Close()
		gz.Close()
		w.Write(buf.Bytes())
	case strings.HasPrefix(p, "/repos/acme/skills/compare/"):
		a, b, _ := strings.Cut(strings.TrimPrefix(p, "/repos/acme/skills/compare/"), "...")
		from, to := f.commits[a], f.commits[b]
		var out []map[string]any
		for k, v := range to {
			if old, ok := from[k]; !ok {
				out = append(out, map[string]any{"filename": k, "status": "added", "additions": 1})
			} else if old != v {
				out = append(out, map[string]any{"filename": k, "status": "modified", "additions": 1, "deletions": 1})
			}
		}
		for k := range from {
			if _, ok := to[k]; !ok {
				out = append(out, map[string]any{"filename": k, "status": "removed", "deletions": 1})
			}
		}
		js(map[string]any{"files": out})
	default:
		http.NotFound(w, r)
	}
}

func skillMD(name, desc string) fakeFile {
	return fakeFile{body: "---\nname: " + name + "\ndescription: " + desc + "\n---\n\nDo it.\n"}
}

func TestASkillIsFetchedFromGitHubAndUpdatedWhenAsked(t *testing.T) {
	gh, s := newFakeGitHub(t)
	v1 := gh.push("main", map[string]fakeFile{
		"README.md":                  {body: "# skills\n"},
		"skills/pdf/SKILL.md":        skillMD("pdf", "Read PDFs."),
		"skills/pdf/scripts/fill.py": {body: "print(1)\n", exec: true},
		"skills/docx/SKILL.md":       skillMD("docx", "Write Word files."),
		"skills/sheets/SKILL.md":     skillMD("xlsx", "Spreadsheets."),
		"skills/linky/SKILL.md":      skillMD("linky", "Has a link."),
		"skills/linky/run":           {link: "/etc/passwd"},
		"node_modules/x/SKILL.md":    skillMD("nm", "Not looked at."),
	})

	env := map[string]any{"github": "https://github.com/acme/skills"}
	if _, err := s.Normalize("pdf", env); err != nil {
		t.Fatal(err)
	}
	o, ok := originOf(env)
	if !ok || o.Repo != "acme/skills" || o.Ref != "main" || o.Path != "skills/pdf" || o.Commit != v1 || o.Tree == "" || o.Digest == "" {
		t.Fatalf("origin %+v from %v", o, env)
	}
	if env["github"] != nil || env["files"] != 2 || env["description"] != "Read PDFs." {
		t.Fatalf("stored %v", env)
	}
	if st, err := os.Stat(filepath.Join(s.Dir, "pdf/scripts/fill.py")); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("the script was not kept executable: %v", err)
	}
	// A skill's folder need not be called as it is: its name finds it.
	if _, err := s.Normalize("xlsx", map[string]any{"github": "acme/skills"}); err != nil {
		t.Fatal(err)
	}
	// One the repository does not have is refused with what it has.
	_, err := s.Normalize("pptx", map[string]any{"github": "acme/skills"})
	if err == nil || !strings.Contains(err.Error(), "it has: docx, linky, pdf, xlsx") {
		t.Fatalf("%v", err)
	}
	// A link is refused, as in a folder.
	if _, err := s.Normalize("linky", map[string]any{"github": "acme/skills/skills/linky"}); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Fatalf("a link was taken: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "linky")); err == nil {
		t.Fatal("a refused skill was kept")
	}
	if _, err := s.Normalize("pdf", map[string]any{"github": "acme/nothing"}); err == nil || !strings.Contains(err.Error(), "public repositories only") {
		t.Fatalf("%v", err)
	}

	cfg, _ := json.Marshal(env)
	out, err := s.Check(context.Background(), "pdf", cfg, nil)
	if err != nil || !strings.Contains(out, "up to date with main") {
		t.Fatalf("%s %v", out, err)
	}
	if st := s.Status("pdf", cfg); st == nil || len(st.Chips) != 1 || st.Chips[0].Text != "up to date" {
		t.Fatalf("%+v", st)
	}

	// A push outside the skill's folder is no update.
	gh.push("main", map[string]fakeFile{
		"README.md":                  {body: "# skills, now with more\n"},
		"skills/pdf/SKILL.md":        skillMD("pdf", "Read PDFs."),
		"skills/pdf/scripts/fill.py": {body: "print(1)\n", exec: true},
	})
	if out, _ := s.Check(context.Background(), "pdf", cfg, nil); !strings.Contains(out, "up to date") {
		t.Fatalf("a change elsewhere in the repository was an update: %s", out)
	}
	v3 := gh.push("main", map[string]fakeFile{
		"skills/pdf/SKILL.md":         skillMD("pdf", "Read and fill PDFs."),
		"skills/pdf/scripts/fill.py":  {body: "print(1)\n", exec: true},
		"skills/pdf/scripts/merge.py": {body: "print(2)\n", exec: true},
	})
	out, _ = s.Check(context.Background(), "pdf", cfg, nil)
	for _, want := range []string{"update available: " + v1[:7] + " → " + v3[:7] + ", 2 files changed", "M SKILL.md", "A scripts/merge.py", "--update"} {
		if !strings.Contains(out, want) {
			t.Errorf("check says %q, not %q", out, want)
		}
	}
	if st := s.Status("pdf", cfg); st.Action != "update" || st.Chips[0].Text != "update available" {
		t.Fatalf("%+v", st)
	}
	// Nothing changed until asked.
	if b, _ := os.ReadFile(filepath.Join(s.Dir, "pdf/SKILL.md")); !strings.Contains(string(b), "Read PDFs.") {
		t.Fatal("the kept copy changed without --update")
	}

	// An edit on the Mac is not replaced without --force.
	os.WriteFile(filepath.Join(s.Dir, "pdf/SKILL.md"), []byte("---\nname: pdf\ndescription: Mine.\n---\n\nMine.\n"), 0o644)
	if out, _ := s.Check(context.Background(), "pdf", cfg, nil); !strings.Contains(out, "changed on the Mac") {
		t.Fatalf("%s", out)
	}
	upd := map[string]any{}
	json.Unmarshal(cfg, &upd)
	upd["update"] = true
	if _, err := s.Normalize("pdf", upd); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("an edit on the Mac was replaced: %v", err)
	}
	upd = map[string]any{}
	json.Unmarshal(cfg, &upd)
	upd["update"], upd["force"] = true, true
	if _, err := s.Normalize("pdf", upd); err != nil {
		t.Fatal(err)
	}
	if o, _ := originOf(upd); o.Commit != v3 || upd["update"] != nil || upd["description"] != "Read and fill PDFs." || upd["files"] != 3 {
		t.Fatalf("%v", upd)
	}

	// Pinned to a commit, there is nothing to follow.
	pin := map[string]any{}
	cfg, _ = json.Marshal(upd)
	json.Unmarshal(cfg, &pin)
	pin["ref"] = v1[:12]
	if _, err := s.Normalize("pdf", pin); err != nil {
		t.Fatal(err)
	}
	cfg, _ = json.Marshal(pin)
	if out, _ := s.Check(context.Background(), "pdf", cfg, nil); !strings.Contains(out, "pinned to a commit") {
		t.Fatalf("%s", out)
	}

	// Copied in from a folder, the GitHub source is forgotten.
	local := skill(t, map[string]string{"SKILL.md": "---\nname: pdf\ndescription: Local.\n---\n\nHere.\n"})
	pin["path"] = local
	if _, err := s.Normalize("pdf", pin); err != nil {
		t.Fatal(err)
	}
	if _, ok := originOf(pin); ok || pin["source"] == nil {
		t.Fatalf("%v", pin)
	}
	pin["update"] = true
	if _, err := s.Normalize("pdf", pin); err == nil {
		t.Fatal("a skill from a folder was updated from GitHub")
	}
}

func TestAnAddressNamesTheBranchAndFolder(t *testing.T) {
	gh, s := newFakeGitHub(t)
	gh.push("main", map[string]fakeFile{"skills/pdf/SKILL.md": skillMD("pdf", "Main's.")})
	gh.push("feature/x", map[string]fakeFile{"skills/pdf/SKILL.md": skillMD("pdf", "The feature's.")})
	env := map[string]any{"github": "https://github.com/acme/skills/tree/feature/x/skills/pdf"}
	if _, err := s.Normalize("pdf", env); err != nil {
		t.Fatal(err)
	}
	if o, _ := originOf(env); o.Ref != "feature/x" || o.Path != "skills/pdf" || env["description"] != "The feature's." {
		t.Fatalf("%v", env)
	}
	env = map[string]any{"github": "https://github.com/acme/skills/blob/main/skills/pdf/SKILL.md"}
	if _, err := s.Normalize("pdf", env); err != nil || env["description"] != "Main's." {
		t.Fatalf("%v %v", env, err)
	}
	if _, err := s.Normalize("pdf", map[string]any{"github": "acme/skills", "ref": "nope"}); err == nil || !strings.Contains(err.Error(), "no branch, tag or commit nope") {
		t.Fatalf("%v", err)
	}

	cs, err := s.Choices(context.Background(), "github", map[string]any{"github": "acme/skills", "ref": "feature/x"}, nil)
	if err != nil || len(cs) != 1 || cs[0].Name != "pdf" || cs[0].Value != "acme/skills/skills/pdf" || cs[0].Note != "The feature's." {
		t.Fatalf("%+v %v", cs, err)
	}
}
