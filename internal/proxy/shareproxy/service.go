// Package shareproxy is share: a folder of the Mac's in the isolation, at
// /shared/<env>. It is not a proxy either. container mounts the folder into
// the VM (virtiofs), so there is no port and nothing in between: an agent
// reads and, when the env says so, writes the Mac's files directly.
//
// That is a hole in the isolation, and it is drawn to be a small one:
//
//   - read-only unless the env says read_write. A read-only mount is
//     refused by the VM's kernel (measured: "Read-only file system").
//   - a writable share gives agents the Mac user's own authority over that
//     folder. The VM does not enforce file permissions there (measured: an
//     agent overwrote a 0600 file and planted an executable git hook), and
//     on the Mac everything is the user's. What runs from that folder on
//     the Mac (hooks, scripts, a Makefile) is then the agents' to write.
//   - only a folder inside the home folder, and never the home folder, a
//     dot folder of it (ssh, aws and gero's own keys live there), Library,
//     or gero's own directories. Desktop, Documents and Downloads are
//     refused too: macOS keeps the container runtime out of them, and the
//     VM would not start (measured: "Operation not permitted").
//
// The folder is checked again whenever a container is started with it: one
// that has gone, or has been replaced by a link elsewhere, is left out.
//
// container mounts a folder only when it starts a container, so turning a
// share on or off, or changing its folder, replaces the isolation's
// container (proxy.Mounter): its volumes are kept, and running agents end
// and start again on their next message.
package shareproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/paths"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

const Name = "share"

// Env is one shared folder.
type Env struct {
	Path      string `json:"path"`
	ReadWrite bool   `json:"read_write"`
}

// Service is share. Home and Protected are what a folder is checked
// against; empty, they are the user's home folder and gero's own
// directories.
type Service struct {
	Home      string
	Protected []string
}

func (Service) Name() string { return Name }
func (Service) Ports() []int { return nil }
func (Service) Summary() string {
	return "a folder of the Mac's in the isolation at /shared/NAME, read-only unless --read-write; turning it on or off restarts the isolation"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "path", Kind: proxy.String, Flag: "path", Metavar: "DIR", Path: true,
			Help: "the Mac's folder: inside your home folder, not a dot folder, Library, Desktop, Documents or Downloads (required)"},
		{Key: "read_write", Kind: proxy.Bool, Flag: "read-write", Neg: "read-only",
			Help: "let agents change and delete what is in it, as you (default read-only)"},
	}
}

func (Service) Settings() []proxy.Field                { return nil }
func (Service) NormalizeSettings(map[string]any) error { return nil }

func (s Service) Normalize(name string, env map[string]any) ([]string, error) {
	path, err := s.resolve(proxy.StringOr(env, "path", ""))
	if err != nil {
		return nil, err
	}
	rw, _ := env["read_write"].(bool)
	env["path"], env["read_write"] = path, rw
	if rw {
		return []string{fmt.Sprintf("%s is shared read-write: agents in an isolation that has it can change and delete anything in it, as you, "+
			"and what they put there (a git hook, a script) runs on this Mac when something there runs it", path)}, nil
	}
	return nil, nil
}

func (s Service) home() string {
	h := s.Home
	if h == "" {
		h, _ = os.UserHomeDir()
	}
	return real(h)
}

func (s Service) protected() []string {
	l := s.Protected
	if l == nil {
		p := paths.Default()
		l = []string{p.Config, p.Data, p.State}
	}
	out := make([]string, 0, len(l))
	for _, p := range l {
		out = append(out, real(p))
	}
	return out
}

// real is a path with its links followed, as far as it exists.
func real(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// resolve is the folder a path names, links followed, or why it cannot be
// shared.
func (s Service) resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", proxy.Deny("--path is required: the Mac's folder to share")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(s.home(), p[1:])
	}
	if !filepath.IsAbs(p) {
		return "", proxy.Deny("%q is not an absolute path", p)
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", proxy.Deny("%s does not exist", p)
		}
		return "", proxy.Deny("%s: %v", p, err)
	}
	if fi, err := os.Stat(r); err != nil || !fi.IsDir() {
		return "", proxy.Deny("%s is not a folder", p)
	}
	if strings.Contains(r, ",") {
		return "", proxy.Deny("%s has a comma in it, which container's --mount cannot carry", r)
	}
	return r, s.allowed(r)
}

// allowed refuses a folder that is never shared. The Mac's disk is
// case-insensitive, so names are compared that way.
func (s Service) allowed(r string) error {
	home := s.home()
	rel, err := filepath.Rel(strings.ToLower(home), strings.ToLower(r))
	switch {
	case err != nil, rel == "..", strings.HasPrefix(rel, "../"):
		return proxy.Deny("%s is not inside your home folder (%s): only a folder in it is shared", r, home)
	case rel == ".":
		return proxy.Deny("your home folder itself is never shared: it holds your keys and credentials; share a folder in it")
	}
	first := strings.Split(rel, "/")[0]
	switch {
	case strings.HasPrefix(first, "."):
		return proxy.Deny("%s is in ~/%s: the dot folders of your home hold configuration and credentials (ssh, aws, gero's own), and are never shared", r, first)
	case first == "library":
		return proxy.Deny("%s is in ~/Library, which holds apps' data, keychains and iCloud Drive, and is never shared", r)
	case first == "desktop", first == "documents", first == "downloads":
		return proxy.Deny("macOS keeps the container runtime out of %s (privacy protection: the VM does not start, \"Operation not permitted\"); share a folder outside Desktop, Documents and Downloads", r)
	}
	low := strings.ToLower(r)
	for _, p := range s.protected() {
		lp := strings.ToLower(p)
		if inside(low, lp) || inside(lp, low) {
			return proxy.Deny("%s holds or is in gero's own %s, with its credentials and the daemon's state, and is never shared", r, p)
		}
	}
	return nil
}

func inside(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// Mounts are the exposed envs' folders, each at /shared/<env>, checked as
// they are now.
func (s Service) Mounts(v proxy.View) (mounts []isolation.Mount, missing []string) {
	for _, name := range v.Exposure.Envs {
		var e Env
		if err := v.Decode(name, &e); err != nil {
			missing = append(missing, name+" ("+err.Error()+")")
			continue
		}
		r, err := s.resolve(e.Path)
		switch {
		case err != nil:
			missing = append(missing, name+" ("+err.Error()+")")
			continue
		case r != e.Path:
			missing = append(missing, fmt.Sprintf("%s (%s is a link to %s now, not the folder it was shared as)", name, e.Path, r))
			continue
		}
		mounts = append(mounts, isolation.Mount{Source: e.Path, Target: isolation.SharedRoot + "/" + name, ReadOnly: !e.ReadWrite})
	}
	for _, name := range v.Missing {
		missing = append(missing, name+" (no such share env)")
	}
	return mounts, missing
}

// TokenKeys: none; nothing answers a request.
func (Service) TokenKeys(store.Exposure) []string { return nil }

// Workspace is the skill, which says what each folder is.
func (s Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	mounts, missing := s.Mounts(v)
	return proxy.Workspace{Files: proxy.SkillFiles(Name, skillBody(mounts, missing))}, nil
}

func (Service) Managed() []string { return proxy.SkillDirs(Name) }

func (s Service) Check(_ context.Context, name string, cfg json.RawMessage, _ *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	r, err := s.resolve(e.Path)
	if err != nil {
		return "", err
	}
	if r != e.Path {
		return "", proxy.Deny("%s is a link to %s now, not the folder it was shared as; define the env again to share that", e.Path, r)
	}
	entries, err := os.ReadDir(r)
	if err != nil {
		return "", err
	}
	mode := "read-only"
	if e.ReadWrite {
		mode = "read-write"
	}
	return fmt.Sprintf("%s: a folder of %d entr%s, shared %s at %s/%s", r, len(entries), map[bool]string{true: "y", false: "ies"}[len(entries) == 1], mode, isolation.SharedRoot, name), nil
}

// Refusals: none it makes itself; `gero iso verify` measures the mounts.
func (Service) Refusals(proxy.View) []proxy.Refusal { return nil }

func (Service) New(proxy.Host) proxy.Instance { return noInstance{} }

type noInstance struct{}

func (noInstance) Serve(port int, l net.Listener) error {
	l.Close()
	return fmt.Errorf("share: no service on port %d", port)
}
