package shareproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// fakeHome is a home folder with gero's directories in it, as the XDG
// layout puts them.
func fakeHome(t *testing.T) (Service, string) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	for _, d := range []string{"Projects/notes/sub", ".ssh", ".config/gero", "Library/Mobile Documents", "Documents/x", "Downloads", "Desktop", "work/.local-not-dot"} {
		os.MkdirAll(filepath.Join(home, d), 0o755)
	}
	return Service{Home: home, Protected: []string{filepath.Join(home, ".config/gero"), filepath.Join(home, "Projects/gero-state")}}, home
}

func normalize(s Service, env map[string]any) (map[string]any, []string, error) {
	w, err := s.Normalize("notes", env)
	return env, w, err
}

func TestAFolderInTheHomeIsSharedReadOnlyByDefault(t *testing.T) {
	s, home := fakeHome(t)
	env, w, err := normalize(s, map[string]any{"path": "~/Projects/notes"})
	if err != nil {
		t.Fatal(err)
	}
	if env["path"] != filepath.Join(home, "Projects/notes") || env["read_write"] != false || len(w) != 0 {
		t.Fatalf("%v %v", env, w)
	}
	// A link is followed: what is stored is the folder itself.
	os.Symlink(filepath.Join(home, "Projects/notes"), filepath.Join(home, "notes-link"))
	env, _, err = normalize(s, map[string]any{"path": filepath.Join(home, "notes-link"), "read_write": true})
	if err != nil {
		t.Fatal(err)
	}
	if env["path"] != filepath.Join(home, "Projects/notes") {
		t.Fatalf("link stored as %v", env["path"])
	}
	_, w, _ = normalize(s, map[string]any{"path": filepath.Join(home, "Projects/notes"), "read_write": true})
	if len(w) != 1 || !strings.Contains(w[0], "read-write") || !strings.Contains(w[0], "git hook") {
		t.Fatalf("a writable share is not warned about: %v", w)
	}
}

func TestFoldersThatAreNeverShared(t *testing.T) {
	s, home := fakeHome(t)
	os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(home, "Projects/keys"))
	os.MkdirAll(filepath.Join(home, "Projects/gero-state/x"), 0o755)
	for path, want := range map[string]string{
		"":                                   "--path is required",
		"Projects/notes":                     "not an absolute path",
		home:                                 "home folder itself",
		home + "/":                           "home folder itself",
		filepath.Join(home, ".ssh"):          "dot folders",
		filepath.Join(home, ".config/gero"):  "dot folders",
		filepath.Join(home, "Projects/keys"): "dot folders", // a link into one
		filepath.Join(home, "Library/Mobile Documents"): "~/Library",
		filepath.Join(home, "Documents/x"):              "privacy protection",
		filepath.Join(home, "downloads"):                "privacy protection", // the disk ignores case
		filepath.Join(home, "Desktop"):                  "privacy protection",
		filepath.Join(home, "Projects/gero-state/x"):    "gero's own",
		filepath.Join(home, "Projects"):                 "gero's own", // it holds gero's directory
		filepath.Dir(home):                              "not inside your home",
		"/etc":                                          "not inside your home",
		filepath.Join(home, "nope"):                     "does not exist",
	} {
		_, _, err := normalize(s, map[string]any{"path": path})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", path, err, want)
		}
	}
	os.WriteFile(filepath.Join(home, "Projects/notes/file"), nil, 0o644)
	if _, _, err := normalize(s, map[string]any{"path": filepath.Join(home, "Projects/notes/file")}); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("a file: %v", err)
	}
	// A folder whose name only starts like a dot folder's is fine.
	if _, _, err := normalize(s, map[string]any{"path": filepath.Join(home, "work/.local-not-dot")}); err != nil {
		t.Errorf("a dot folder below the first level: %v", err)
	}
}

func view(envs map[string]Env, order ...string) proxy.View {
	v := proxy.View{Isolation: "dev", Enabled: true, Exposure: store.Exposure{Service: Name, Envs: order}, Envs: map[string]json.RawMessage{}}
	for n, e := range envs {
		v.Envs[n], _ = json.Marshal(e)
	}
	return v
}

func TestTheMountsAreCheckedAsTheyAreNow(t *testing.T) {
	s, home := fakeHome(t)
	notes := filepath.Join(home, "Projects/notes")
	gone := filepath.Join(home, "Projects/gone")
	os.MkdirAll(gone, 0o755)
	swapped := filepath.Join(home, "Projects/swapped")
	os.MkdirAll(swapped, 0o755)
	v := view(map[string]Env{"notes": {Path: notes}, "out": {Path: notes + "/sub", ReadWrite: true}, "gone": {Path: gone}, "swapped": {Path: swapped}},
		"notes", "out", "gone", "swapped", "removed")
	v.Missing = []string{"removed"}
	os.RemoveAll(gone)
	// Replaced by a link into a folder that is never shared.
	os.RemoveAll(swapped)
	os.Symlink(filepath.Join(home, ".ssh"), swapped)

	mounts, missing := s.Mounts(v)
	want := []isolation.Mount{
		{Source: notes, Target: "/shared/notes", ReadOnly: true},
		{Source: notes + "/sub", Target: "/shared/out"},
	}
	if !reflect.DeepEqual(mounts, want) {
		t.Fatalf("mounts %+v", mounts)
	}
	got := strings.Join(missing, "\n")
	for _, w := range []string{"gone (", "does not exist", "swapped (", "dot folders", "removed (no such share env)"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing does not say %q:\n%s", w, got)
		}
	}
	ws, _ := s.Workspace(v)
	skill := ws.Files[0].Content
	for _, w := range []string{"/shared/notes is the Mac's " + notes + ", read-only", "/shared/out is the Mac's " + notes + "/sub, read-write", "not mounted now: gone"} {
		if !strings.Contains(skill, w) {
			t.Errorf("the skill does not say %q:\n%s", w, skill)
		}
	}
}

func TestCheckSaysWhatIsShared(t *testing.T) {
	s, home := fakeHome(t)
	notes := filepath.Join(home, "Projects/notes")
	cfg, _ := json.Marshal(Env{Path: notes})
	out, err := s.Check(nil, "notes", cfg, nil)
	if err != nil || out != notes+": a folder of 1 entry, shared read-only at /shared/notes" {
		t.Fatalf("%q %v", out, err)
	}
	cfg, _ = json.Marshal(Env{Path: filepath.Join(home, "Projects/none")})
	if _, err := s.Check(nil, "notes", cfg, nil); err == nil {
		t.Fatal("a folder that is gone checked fine")
	}
}
