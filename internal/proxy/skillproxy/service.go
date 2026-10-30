// Package skillproxy is skill: the operator's own skills, put where Claude
// Code and Codex find them in an isolation. It is not a proxy either; there
// is no port and nothing held back. Each env is one skill, kept on the Mac
// in ~/.config/gero/skills/<env>, copied there from the folder it is added
// from, after it is checked (format.go). An isolation that has it exposed
// gets each skill at ~/.claude/skills/gero-skill-<env> and
// ~/.codex/skills/gero-skill-<env>, written by `gero-shim sync` like every
// file gero manages there; both CLIs know a skill by the name in its
// frontmatter, so it is still <env> to the agent.
//
// A skill is handed to agents as it is, scripts included, so it is only as
// trustworthy as where it came from: links are refused rather than
// followed, so nothing outside its folder rides along.
package skillproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/paths"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/store"
)

const Name = "skill"

// roots are the agent CLIs' user skill folders in the isolation, and
// prefix what each of these skills' folders there starts with.
var roots = []string{"/home/agent/.claude/skills", "/home/agent/.codex/skills"}

const prefix = "gero-skill-"

// Service is skill. Dir is where the skills are kept and Protected what a
// skill is never copied from; empty, they are gero's own.
type Service struct {
	Dir       string
	Protected []string
}

func (Service) Name() string { return Name }
func (Service) Ports() []int { return nil }
func (Service) Summary() string {
	return "a skill of your own (a folder with SKILL.md), checked and kept in ~/.config/gero/skills, for Claude Code and Codex in the isolation"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "path", Kind: proxy.String, Flag: "path", Metavar: "DIR", Path: true,
			Help: "the skill's folder, SKILL.md at its top, to copy in (replacing what is kept); left out, the one already in ~/.config/gero/skills/NAME is checked"},
	}
}

func (Service) Settings() []proxy.Field                { return nil }
func (Service) NormalizeSettings(map[string]any) error { return nil }

func (s Service) dir() string {
	if s.Dir != "" {
		return s.Dir
	}
	return paths.Default().Skills()
}

func (s Service) protected() []string {
	if s.Protected != nil {
		return s.Protected
	}
	p := paths.Default()
	return []string{p.Config, p.Data, p.State}
}

// Normalize adds the skill: copied in from path when one is given, checked
// where it is kept when not. What is stored is where it came from and what
// it says it is for; the skill itself is the folder.
func (s Service) Normalize(name string, env map[string]any) ([]string, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	src := strings.TrimSpace(proxy.StringOr(env, "path", ""))
	delete(env, "path")
	var (
		sk  Skill
		err error
	)
	if src != "" {
		if sk, err = s.Import(src, name); err != nil {
			return nil, err
		}
		env["source"] = sk.source
	} else {
		dir := filepath.Join(s.dir(), name)
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			return nil, proxy.Deny("there is no skill %s in %s yet: give --path, the skill's folder to copy in", name, s.dir())
		}
		if sk, err = Load(dir, name); err != nil {
			return nil, err
		}
	}
	env["description"] = sk.Description
	env["files"] = len(sk.Files)
	return sk.Warnings, nil
}

// Import checks the skill in src and, if it passes, makes it the one kept
// as name, all at once: a skill that fails leaves the kept one as it was.
func (s Service) Import(src, name string) (Skill, error) {
	from, err := s.resolve(src)
	if err != nil {
		return Skill{}, err
	}
	sk, err := Load(from, name)
	if err != nil {
		return sk, err
	}
	sk.source = from
	dest := filepath.Join(s.dir(), name)
	if real(dest) == from {
		return sk, nil
	}
	if err := os.MkdirAll(s.dir(), 0o700); err != nil {
		return sk, err
	}
	tmp, err := os.MkdirTemp(s.dir(), ".add-"+name+"-")
	if err != nil {
		return sk, err
	}
	defer os.RemoveAll(tmp)
	os.Chmod(tmp, 0o755)
	for _, f := range sk.Files {
		p := filepath.Join(tmp, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return sk, err
		}
		if err := os.WriteFile(p, f.Content, f.mode()); err != nil {
			return sk, err
		}
	}
	old := ""
	if _, err := os.Lstat(dest); err == nil {
		old = tmp + ".old"
		if err := os.Rename(dest, old); err != nil {
			return sk, err
		}
		defer os.RemoveAll(old)
	}
	if err := os.Rename(tmp, dest); err != nil {
		if old != "" {
			os.Rename(old, dest)
		}
		return sk, err
	}
	return sk, nil
}

func (f File) mode() fs.FileMode {
	if f.Exec {
		return 0o755
	}
	return 0o644
}

// resolve is the folder src names, links followed, or why a skill is not
// taken from it: gero's own directories hold its credentials and state.
func (s Service) resolve(src string) (string, error) {
	if src == "~" || strings.HasPrefix(src, "~/") {
		home, _ := os.UserHomeDir()
		src = filepath.Join(home, src[1:])
	}
	if !filepath.IsAbs(src) {
		return "", proxy.Deny("%q is not an absolute path", src)
	}
	r, err := filepath.EvalSymlinks(src)
	if errors.Is(err, fs.ErrNotExist) {
		return "", proxy.Deny("%s does not exist", src)
	}
	if err != nil {
		return "", proxy.Deny("%s: %v", src, err)
	}
	kept := real(s.dir())
	if inside(strings.ToLower(r), strings.ToLower(kept)) {
		if filepath.Dir(r) != kept {
			return "", proxy.Deny("%s is inside a kept skill; give the skill's own folder", r)
		}
		return r, nil
	}
	for _, p := range s.protected() {
		lp, lr := strings.ToLower(real(p)), strings.ToLower(r)
		if inside(lr, lp) || inside(lp, lr) {
			return "", proxy.Deny("%s holds or is in gero's own %s, with its credentials and state; a skill is not taken from there", r, p)
		}
	}
	return r, nil
}

func real(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func inside(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// TokenKeys: none; nothing answers a request.
func (Service) TokenKeys(store.Exposure) []string { return nil }

// Workspace is every exposed skill, as it is kept now, in both CLIs'
// skill folders. One that no longer passes is left out (and so removed).
func (s Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	var ws proxy.Workspace
	for _, name := range v.Exposure.Envs {
		if _, ok := v.Envs[name]; !ok || CheckName(name) != nil {
			continue
		}
		sk, err := Load(filepath.Join(s.dir(), name), name)
		if err != nil {
			continue
		}
		for _, root := range roots {
			for _, f := range sk.Files {
				sf := shimproto.SyncFile{Path: path.Join(root, prefix+name, f.Path), Mode: uint32(f.mode()), Agent: true}
				if f.Text() {
					sf.Content = string(f.Content)
				} else {
					sf.Content, sf.Base64 = base64.StdEncoding.EncodeToString(f.Content), true
				}
				ws.Files = append(ws.Files, sf)
			}
		}
	}
	return ws, nil
}

// Prune: whatever of these folders the workspace does not write goes.
func (Service) Prune() []string {
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = r + "/" + prefix
	}
	return out
}

// Managed is every kept skill's folders, for a shim too old to prune.
func (s Service) Managed() []string {
	entries, _ := os.ReadDir(s.dir())
	var out []string
	for _, e := range entries {
		if e.IsDir() && CheckName(e.Name()) == nil {
			for _, r := range roots {
				out = append(out, r+"/"+prefix+e.Name())
			}
		}
	}
	return out
}

// Deleted removes the kept skill along with its env.
func (s Service) Deleted(name string) error {
	if CheckName(name) != nil {
		return nil
	}
	return os.RemoveAll(filepath.Join(s.dir(), name))
}

func (s Service) Check(_ context.Context, name string, _ json.RawMessage, _ *creds.Store) (string, error) {
	dir := filepath.Join(s.dir(), name)
	sk, err := Load(dir, name)
	if err != nil {
		return "", err
	}
	out := fmt.Sprintf("%s: %d file%s, %s, kept in %s: %s", name, len(sk.Files), map[bool]string{true: "", false: "s"}[len(sk.Files) == 1], size(sk.Size()), dir, sk.Description)
	for _, w := range sk.Warnings {
		out += "\nwarning: " + w
	}
	return out, nil
}

func size(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}

// Refusals: none; it answers nothing.
func (Service) Refusals(proxy.View) []proxy.Refusal { return nil }

func (Service) New(proxy.Host) proxy.Instance { return noInstance{} }

type noInstance struct{}

func (noInstance) Serve(port int, l net.Listener) error {
	l.Close()
	return fmt.Errorf("skill: no service on port %d", port)
}
