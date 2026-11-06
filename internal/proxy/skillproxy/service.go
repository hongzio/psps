// Package skillproxy is skill: the operator's own skills, put where Claude
// Code and Codex find them in an isolation. It is not a proxy either; there
// is no port and nothing held back. Each env is one skill, kept on the Mac
// in ~/.config/psps/skills/<env>, copied there from the folder it is added
// from or fetched from a public GitHub repository (github.go), after it is
// checked (format.go). An isolation that has it exposed
// gets each skill at ~/.claude/skills/psps-skill-<env> and
// ~/.codex/skills/psps-skill-<env>, written by `psps-shim sync` like every
// file psps manages there; both CLIs know a skill by the name in its
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
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/paths"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/shimproto"
	"github.com/hongzio/psps/internal/store"
)

const Name = "skill"

// roots are the agent CLIs' user skill folders in the isolation, and
// prefix what each of these skills' folders there starts with.
var roots = []string{"/home/agent/.claude/skills", "/home/agent/.codex/skills"}

const prefix = "psps-skill-"

// Service is skill. Dir is where the skills are kept and Protected what a
// skill is never copied from; empty, they are psps's own. API, Raw and
// Client are GitHub's, for tests; empty, the real ones.
type Service struct {
	Dir       string
	Protected []string
	API, Raw  string
	Client    *http.Client
}

func (Service) Name() string { return Name }
func (Service) Ports() []int { return nil }
func (Service) Summary() string {
	return "a skill of your own (a folder with SKILL.md, on the Mac or in a public GitHub repository), checked and kept in ~/.config/psps/skills, for Claude Code and Codex in the isolation"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "path", Kind: proxy.String, Flag: "path", Metavar: "DIR", Path: true,
			Help: "the skill's folder, SKILL.md at its top, to copy in (replacing what is kept); left out, the one already in ~/.config/psps/skills/NAME is checked"},
		{Key: "github", Kind: proxy.String, Flag: "github", Metavar: "OWNER/REPO[/PATH]", Lookup: true, LookupWith: []string{"ref"},
			Help: "instead of --path: a public GitHub repository to fetch the skill from, as owner/repo (the skill called NAME in it), owner/repo/PATH (its folder) or a github.com address"},
		{Key: "ref", Kind: proxy.String, Flag: "ref", Metavar: "REF",
			Help: "with --github: the branch, tag or commit (default: the repository's default branch); on a skill from GitHub, follow REF instead and fetch it now"},
		{Key: "update", Kind: proxy.Bool, Flag: "update",
			Help: "on a skill from GitHub: fetch what its ref has now, checked, in place of what is kept"},
		{Key: "force", Kind: proxy.Bool, Flag: "force",
			Help: "with --update or --ref: replace the kept copy even though it was changed on the Mac since it was fetched"},
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

// Normalize adds the skill: copied in from path or fetched from GitHub
// when one is given, fetched again from where it came from on --update,
// checked where it is kept when neither. What is stored is where it came
// from and what it says it is for; the skill itself is the folder.
func (s Service) Normalize(name string, env map[string]any) ([]string, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	src := strings.TrimSpace(proxy.StringOr(env, "path", ""))
	gh := strings.TrimSpace(proxy.StringOr(env, "github", ""))
	ref := strings.TrimSpace(proxy.StringOr(env, "ref", ""))
	update, force := proxy.BoolOr(env, "update", false), proxy.BoolOr(env, "force", false)
	for _, k := range []string{"path", "github", "ref", "update", "force"} {
		delete(env, k)
	}
	origin, fromGitHub := originOf(env)
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	var (
		sk       Skill
		warnings []string
		err      error
	)
	switch {
	case src != "" && gh != "":
		return nil, proxy.Deny("give --path or --github, not both: they are two places the skill could come from")
	case src != "":
		if ref != "" || update {
			return nil, proxy.Deny("--ref and --update are for a skill from GitHub; --path copies the folder as it is now")
		}
		if sk, err = s.Import(src, name); err != nil {
			return nil, err
		}
		env["source"] = sk.source
		delete(env, "origin")
		warnings = sk.Warnings
	case gh != "":
		sp, err := ParseSpec(gh)
		if err != nil {
			return nil, err
		}
		if sk, origin, warnings, err = s.fetch(ctx, name, sp, ref); err != nil {
			return nil, err
		}
		if err := s.install(sk, name); err != nil {
			return nil, err
		}
		env["origin"] = origin.env()
		delete(env, "source")
	case update || ref != "":
		if !fromGitHub {
			return nil, proxy.Deny("%s was not fetched from GitHub, so there is nothing to update it from: give --github OWNER/REPO, or --path to copy a folder in again", name)
		}
		if !force && s.changedHere(name, origin) {
			return nil, proxy.Deny("%s was changed in %s since it was fetched; updating replaces that change: give --force to replace it", name, filepath.Join(s.dir(), name))
		}
		sp := Spec{Repo: origin.Repo, Ref: origin.Ref, Path: origin.Path, Exact: true}
		if sk, origin, warnings, err = s.fetch(ctx, name, sp, ref); err != nil {
			return nil, fmt.Errorf("%s was left as it was: %w", name, err)
		}
		if err := s.install(sk, name); err != nil {
			return nil, err
		}
		env["origin"] = origin.env()
	default:
		if force {
			return nil, proxy.Deny("--force goes with --update or --ref")
		}
		dir := filepath.Join(s.dir(), name)
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			return nil, proxy.Deny("there is no skill %s in %s yet: give --path, the skill's folder to copy in, or --github, a repository to fetch it from", name, s.dir())
		}
		if sk, err = Load(dir, name); err != nil {
			return nil, err
		}
		warnings = sk.Warnings
	}
	env["description"] = sk.Description
	env["files"] = len(sk.Files)
	forget(filepath.Join(s.dir(), name))
	return warnings, nil
}

// changedHere is whether the kept copy is not what was fetched.
func (s Service) changedHere(name string, o Origin) bool {
	sk, err := Load(filepath.Join(s.dir(), name), name)
	return err == nil && o.Digest != "" && digest(sk.Files) != o.Digest
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
	if real(filepath.Join(s.dir(), name)) == from {
		return sk, nil
	}
	return sk, s.install(sk, name)
}

// install makes sk, already checked, the one kept as name, all at once.
func (s Service) install(sk Skill, name string) error {
	dest := filepath.Join(s.dir(), name)
	if err := os.MkdirAll(s.dir(), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(s.dir(), ".add-"+name+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	os.Chmod(tmp, 0o755)
	for _, f := range sk.Files {
		p := filepath.Join(tmp, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, f.Content, f.mode()); err != nil {
			return err
		}
	}
	old := ""
	if _, err := os.Lstat(dest); err == nil {
		old = tmp + ".old"
		if err := os.Rename(dest, old); err != nil {
			return err
		}
		defer os.RemoveAll(old)
	}
	if err := os.Rename(tmp, dest); err != nil {
		if old != "" {
			os.Rename(old, dest)
		}
		return err
	}
	return nil
}

func (f File) mode() fs.FileMode {
	if f.Exec {
		return 0o755
	}
	return 0o644
}

// resolve is the folder src names, links followed, or why a skill is not
// taken from it: psps's own directories hold its credentials and state.
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
			return "", proxy.Deny("%s holds or is in psps's own %s, with its credentials and state; a skill is not taken from there", r, p)
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

func (s Service) Check(ctx context.Context, name string, cfg json.RawMessage, _ *creds.Store) (string, error) {
	dir := filepath.Join(s.dir(), name)
	sk, err := Load(dir, name)
	if err != nil {
		return "", err
	}
	out := fmt.Sprintf("%s: %d file%s, %s, kept in %s: %s", name, len(sk.Files), map[bool]string{true: "", false: "s"}[len(sk.Files) == 1], size(sk.Size()), dir, sk.Description)
	for _, w := range sk.Warnings {
		out += "\nwarning: " + w
	}
	var env map[string]any
	json.Unmarshal(cfg, &env)
	if o, ok := originOf(env); ok {
		out += "\n" + s.look(ctx, name, o).report(name, o)
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
