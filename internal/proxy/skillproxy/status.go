package skillproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/psps/internal/proxy"
)

// A skill from GitHub is looked at again, against what its ref has now,
// when someone checks it, and otherwise once a day while its env is
// listed; what was found is kept in memory, for the list to show.

const recheck = 24 * time.Hour

// state is one look at a skill from GitHub.
type state struct {
	At time.Time
	// Commit is the kept commit the look was for: an update makes it
	// stale.
	Commit string
	// Changed here: the kept copy is not what was fetched.
	ChangedHere bool
	// Latest is the commit its ref names now, when its folder differs
	// there; Files what differs.
	Latest string
	Files  []Changed
	// Unreachable is why the repository could not be asked.
	Unreachable string
}

var looks = struct {
	sync.Mutex
	m       map[string]state
	running map[string]bool
}{m: map[string]state{}, running: map[string]bool{}}

// forget drops what was found for the skill kept in dir.
func forget(dir string) {
	looks.Lock()
	delete(looks.m, dir)
	looks.Unlock()
}

// look asks GitHub about the skill called name, now, and keeps the answer.
func (s Service) look(ctx context.Context, name string, o Origin) state {
	st := state{At: time.Now(), Commit: o.Commit, ChangedHere: s.changedHere(name, o)}
	if !o.Pinned() {
		g := s.github()
		sha, top, err := g.commit(ctx, o.Repo, o.Ref)
		var tree string
		if err == nil {
			tree, err = g.tree(ctx, o.Repo, sha, top, o.Path)
		}
		switch {
		case err != nil:
			st.Unreachable = unreachable(o, err)
		case tree != o.Tree:
			st.Latest = sha
			st.Files, _ = g.compare(ctx, o.Repo, o.Commit, sha, o.Path)
		}
	}
	looks.Lock()
	looks.m[filepath.Join(s.dir(), name)] = st
	looks.Unlock()
	return st
}

func unreachable(o Origin, err error) string {
	if err == errMissing {
		return fmt.Sprintf("GitHub has no %s @ %s now: it was removed, renamed or made private", o.Repo, o.Ref)
	}
	return err.Error()
}

// Status is a skill from GitHub as last looked at: up to date, an update
// to review, changed on the Mac. A look older than a day, or none, is
// made in the background, for the next listing.
func (s Service) Status(name string, cfg json.RawMessage) *proxy.EnvStatus {
	var env map[string]any
	if json.Unmarshal(cfg, &env) != nil {
		return nil
	}
	o, ok := originOf(env)
	if !ok {
		return nil
	}
	dir := filepath.Join(s.dir(), name)
	looks.Lock()
	st, have := looks.m[dir]
	fresh := have && st.Commit == o.Commit && time.Since(st.At) < recheck
	if !fresh && !looks.running[dir] {
		looks.running[dir] = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
			defer cancel()
			s.look(ctx, name, o)
			looks.Lock()
			delete(looks.running, dir)
			looks.Unlock()
		}()
	}
	looks.Unlock()
	if !have || st.Commit != o.Commit {
		return &proxy.EnvStatus{}
	}
	return st.status(o)
}

func (st state) status(o Origin) *proxy.EnvStatus {
	out := &proxy.EnvStatus{CheckedAt: st.At.UTC().Truncate(time.Second)}
	if st.ChangedHere {
		out.Chips = append(out.Chips, proxy.StatusChip{Text: "changed on the Mac", Kind: "warn"})
	}
	switch {
	case st.Unreachable != "":
		out.Chips = append(out.Chips, proxy.StatusChip{Text: "source unreachable", Kind: "error"})
		out.Detail = st.Unreachable
	case st.Latest != "":
		out.Chips = append(out.Chips, proxy.StatusChip{Text: "update available", Kind: "warn"})
		out.Action = "update"
		out.Detail = fmt.Sprintf("%s → %s, %s", short(o.Commit), short(st.Latest), files(len(st.Files)))
	case o.Pinned():
		out.Chips = append(out.Chips, proxy.StatusChip{Text: "pinned to " + short(o.Ref), Kind: "plain"})
	default:
		out.Chips = append(out.Chips, proxy.StatusChip{Text: "up to date", Kind: "ok"})
	}
	return out
}

func files(n int) string {
	if n == 1 {
		return "1 file changed"
	}
	return fmt.Sprintf("%d files changed", n)
}

// report is what Check says of where a skill came from.
func (st state) report(name string, o Origin) string {
	var b strings.Builder
	fmt.Fprintf(&b, "from %s, kept at %s (fetched %s)", o.where(), short(o.Commit), o.FetchedAt.Local().Format("Jan 2 15:04"))
	switch {
	case st.Unreachable != "":
		fmt.Fprintf(&b, "\nsource unreachable: %s; the kept copy stays in use", st.Unreachable)
	case st.Latest != "":
		fmt.Fprintf(&b, "\nupdate available: %s → %s, %s in %s", short(o.Commit), short(st.Latest), files(len(st.Files)), orTop(o.Path))
		for _, f := range st.Files {
			fmt.Fprintf(&b, "\n  %s %s  +%d −%d", statusLetter(f.Status), f.Path, f.Additions, f.Deletions)
		}
		fmt.Fprintf(&b, "\nhttps://github.com/%s/compare/%s...%s", o.Repo, short(o.Commit), short(st.Latest))
		fmt.Fprintf(&b, "\ntake it with: psps env edit skill %s --update", name)
	case o.Pinned():
		b.WriteString("\npinned to a commit: nothing to follow (--ref BRANCH to follow one)")
	default:
		fmt.Fprintf(&b, "\nup to date with %s", o.Ref)
	}
	if st.ChangedHere {
		fmt.Fprintf(&b, "\nchanged on the Mac since it was fetched: an update replaces that change, and asks for --force first")
	}
	return b.String()
}

func statusLetter(s string) string {
	switch s {
	case "added":
		return "A"
	case "removed":
		return "D"
	case "renamed":
		return "R"
	}
	return "M"
}

// Choices are the skills a GitHub source has, for a form to pick one:
// each as the source that names its folder, with its name.
func (s Service) Choices(ctx context.Context, field string, env map[string]any, _ map[string]string) ([]proxy.Choice, error) {
	if field != "github" {
		return nil, proxy.Deny("skill has no choices for %s", field)
	}
	gh := strings.TrimSpace(proxy.StringOr(env, "github", ""))
	if gh == "" {
		return nil, proxy.Deny("give a GitHub repository first")
	}
	sp, err := ParseSpec(gh)
	if err != nil {
		return nil, err
	}
	g := s.github()
	r, err := g.resolve(ctx, sp, strings.TrimSpace(proxy.StringOr(env, "ref", "")))
	if err != nil {
		return nil, err
	}
	var found []Found
	if r.Exact {
		f := Found{Path: r.Path}
		g.frontmatter(ctx, r, &f)
		if f.Name == "" {
			return nil, proxy.Deny("%s has no readable SKILL.md in %s at %s", r.Repo, orTop(r.Path), short(r.commit))
		}
		found = []Found{f}
	} else if found, err = g.skills(ctx, r, ""); err != nil {
		return nil, err
	}
	out := make([]proxy.Choice, 0, len(found))
	for _, f := range found {
		v := strings.TrimSuffix(r.Repo+"/"+f.Path, "/")
		if sp.Ref != "" || len(sp.refPath) > 0 {
			v += "@" + r.Ref
		}
		out = append(out, proxy.Choice{Value: v, Name: f.Name, Note: f.Description,
			Where: fmt.Sprintf("%s @ %s · %s", r.Repo, r.Ref, short(r.commit))})
	}
	return out, nil
}
