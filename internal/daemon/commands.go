package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/proxy/skillproxy"
)

// Commands, for a message box to offer as they are typed: psps's own, which
// a message that is one of runs instead of being sent, then those of the
// agent's CLI, as its process last told its driver. Files are the
// workspace's, for a message to name one with @.

// pspsCommands are the ones psps runs itself: the agent's settings, which
// are psps's to keep (what its CLI changes of them lasts only until psps
// next starts it), and stopping its turn. models are the ones its CLI
// said it runs.
func pspsCommands(kind driver.Kind, models []driver.Choice) []api.Command {
	ms := []driver.Choice{{Value: "default", Description: "the CLI's default"}}
	for _, md := range models {
		if md.Value != "default" {
			ms = append(ms, md)
		}
	}
	efforts := []driver.Choice{{Value: "default", Description: "the CLI's default"}}
	for _, e := range driver.Efforts(kind) {
		efforts = append(efforts, driver.Choice{Value: e})
	}
	cmd := func(name, desc, hint string, args []driver.Choice) api.Command {
		return api.Command{Command: driver.Command{Prefix: "/", Name: name, Description: desc, Hint: hint, Args: args}, Psps: true}
	}
	return []api.Command{
		cmd("model", "Set the model psps runs it with", "<model>", ms),
		cmd("effort", "Set how hard it thinks", "<level>", efforts),
		cmd("approvals", "Set what it asks you first", "<mode>", []driver.Choice{
			{Value: "never", Description: "run unattended"},
			{Value: "auto", Description: "a reviewer model refuses risky actions"},
			{Value: "ask", Description: "it asks first"},
			{Value: "default", Description: "the CLI's own settings decide"},
		}),
		cmd("btw", "Ask on the side: a subagent forked from this conversation answers with its tools, in the drawer's Subagents", "<question>", nil),
		cmd("stop", "Stop its turn", "", nil),
		cmd("new", "Start over in a new session: same ID and directory, this one archived", "", nil),
	}
}

func (m *Manager) Commands(id string) (api.Commands, error) {
	r, err := m.runtime(id)
	if err != nil {
		return api.Commands{}, err
	}
	kind, cmds, models := r.commands()
	out := pspsCommands(kind, models)
	for _, c := range slices.Concat(cmds, m.skillCommands(kind, r.isolationID())) {
		if !slices.ContainsFunc(out, func(g api.Command) bool { return g.Prefix == c.Prefix && g.Name == c.Name }) {
			out = append(out, api.Command{Command: c})
		}
	}
	return api.Commands{Commands: out}, nil
}

// skillCommands are the skills the isolation (by ID) has from skill, as
// its CLI names them: the CLI lists them only as a process starts, and an
// agent has none until it is next sent something.
func (m *Manager) skillCommands(kind driver.Kind, iso string) []driver.Command {
	svc, err := m.registry().Get(skillproxy.Name)
	if err != nil {
		return nil
	}
	v, err := m.viewFor(iso, svc)
	if err != nil || !v.Enabled {
		return nil
	}
	prefix := "/"
	if kind == driver.Codex {
		prefix = "$"
	}
	var out []driver.Command
	for _, n := range v.Exposure.Envs {
		if raw, ok := v.Envs[n]; ok {
			var cfg struct{ Description string }
			json.Unmarshal(raw, &cfg)
			out = append(out, driver.Command{Prefix: prefix, Name: n, Description: cfg.Description})
		}
	}
	return out
}

func (r *runtime) isolationID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agent.IsolationID
}

func (r *runtime) commands() (driver.Kind, []driver.Command, []driver.Choice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agent.Kind, r.drv.Commands(), r.drv.Models()
}

// pspsCommand reads a message that is one of psps's commands: one line, the
// command and what it takes.
func pspsCommand(text string) (name string, args []string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") || strings.Contains(t, "\n") {
		return "", nil, false
	}
	f := strings.Fields(t)
	switch name = f[0][1:]; name {
	case "model", "effort", "approvals", "stop", "new":
		return name, f[1:], true
	}
	return "", nil, false
}

// Message sends text to the agent, or does what it says instead when it is
// one of psps's commands.
func (m *Manager) Message(id, text string) (api.Sent, error) {
	if q, ok := btwQuestion(text); ok {
		a, err := m.Btw(m.ctx, id, q)
		if err != nil {
			return api.Sent{}, err
		}
		return api.Sent{Done: "Asked on the side: its answer is in the drawer's Subagents.", Agent: a.ID, Side: true}, nil
	}
	name, args, ok := pspsCommand(text)
	if !ok {
		return api.Sent{}, m.Send(id, text)
	}
	r, err := m.runtime(id)
	if err != nil {
		return api.Sent{}, err
	}
	if name == "new" {
		if len(args) > 0 {
			return api.Sent{}, badRequest(errors.New("/new takes nothing"))
		}
		a, err := m.Renew(m.ctx, r.agent.ID)
		if err != nil {
			return api.Sent{}, err
		}
		return api.Sent{Done: "Started a new session; the last one is archived.", Agent: a.ID}, nil
	}
	if name == "stop" {
		if len(args) > 0 {
			return api.Sent{}, badRequest(errors.New("/stop takes nothing"))
		}
		if _, st, _ := r.view(); !st.Busy {
			return api.Sent{Done: "Nothing to stop: it is not working."}, nil
		}
		if err := r.interrupt(); err != nil {
			return api.Sent{}, err
		}
		return api.Sent{Done: "Stopping its turn."}, nil
	}
	if len(args) > 1 {
		return api.Sent{}, badRequest(fmt.Errorf("/%s takes one value", name))
	}
	a, _, _ := r.view()
	o := a.Options
	field := map[string]*string{"model": &o.Model, "effort": &o.Effort, "approvals": (*string)(&o.Approvals)}[name]
	what := strings.ToUpper(name[:1]) + name[1:]
	shown := func(v string) string {
		switch {
		case v != "":
			return v
		case name == "approvals":
			return string(driver.ApprovalsNever)
		}
		return "the CLI's default"
	}
	if len(args) == 0 {
		return api.Sent{Done: what + ": " + shown(*field) + "."}, nil
	}
	v := args[0]
	if v == "default" && name != "approvals" {
		v = ""
	}
	if name == "effort" && v != "" && !slices.Contains(driver.Efforts(a.Kind), v) {
		return api.Sent{}, badRequest(fmt.Errorf("effort is default or one of %s, not %q", strings.Join(driver.Efforts(a.Kind), ", "), v))
	}
	if shown(v) == shown(*field) {
		return api.Sent{Done: what + " is already " + shown(v) + "."}, nil
	}
	was := *field
	*field = v
	if err := m.SetOptions(a.ID, o); err != nil {
		return api.Sent{}, err
	}
	when := "Codex uses it from the next turn."
	if a.Kind == driver.Claude {
		when = "Claude restarts with it when it is idle, with nothing in the background."
		// Between ask and auto the running process switches.
		live := func(s string) bool { return s == string(driver.ApprovalsAsk) || s == string(driver.ApprovalsAuto) }
		if name == "approvals" && live(was) && live(v) {
			when = "Claude switches to it now, in the turn it runs; should it refuse, it restarts with it when it is idle."
		}
	}
	return api.Sent{Done: fmt.Sprintf("%s set to %s. %s", what, shown(v), when)}, nil
}

// isoPath is a path in an isolation as typed: ~ is the agent's home, and
// it must be absolute after that.
func (m *Manager) isoPath(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "~":
		s = m.home
	case strings.HasPrefix(s, "~/"):
		s = m.home + s[1:]
	}
	if strings.ContainsAny(s, "\x00\n") {
		return "", errors.New("a path has no NUL or newline in it")
	}
	if !path.IsAbs(s) {
		return "", fmt.Errorf("%q is not absolute; start it with / or ~", s)
	}
	return path.Clean(s), nil
}

// dirsMax is how many directories a completion offers.
const dirsMax = 200

// dirsScript lists the directories in $1 by name, hidden ones included.
const dirsScript = `cd "$1" 2>/dev/null || exit 0
for d in * .[!.]* ..?*; do [ -d "$d" ] && printf '%s\n' "$d"; done
exit 0`

// Dirs completes a directory typed in the isolation: the directories in
// the part up to its last slash whose names begin with the rest. A hidden
// one is offered once a dot is typed. "" is the home.
// Models are what kind's CLI said it runs in the isolation, or, before it
// has there, in another: a new agent's model is picked from them, before it
// has a process to ask. A model they lack can still be given.
func (m *Manager) Models(isoName string, kind driver.Kind) (api.Models, error) {
	if kind != driver.Claude && kind != driver.Codex {
		return api.Models{}, badRequest(fmt.Errorf("no agent kind %q", kind))
	}
	iso, err := m.isolationNamed(isoName)
	if err != nil {
		return api.Models{}, err
	}
	ms, from, err := m.store.CLIModels(kind, iso.ID)
	if err != nil {
		return api.Models{}, err
	}
	out := api.Models{Models: []driver.Choice{}}
	for _, md := range ms {
		if md.Value != "default" {
			out.Models = append(out.Models, md)
		}
	}
	if from != "" && from != iso.ID {
		out.From = from
		if o, err := m.store.Isolation(from); err == nil {
			out.From = o.Name
		}
	}
	return out, nil
}

func (m *Manager) Dirs(ctx context.Context, isoName, typed string) (api.Dirs, error) {
	iso, err := m.isolationNamed(isoName)
	if err != nil {
		return api.Dirs{}, err
	}
	if strings.TrimSpace(typed) == "" {
		typed = "~/"
	}
	full, err := m.isoPath(typed)
	if err != nil {
		return api.Dirs{}, badRequest(err)
	}
	// Clean took the trailing slash that says "inside this one".
	parent, prefix := path.Split(full)
	if strings.HasSuffix(strings.TrimSpace(typed), "/") {
		parent, prefix = strings.TrimSuffix(full, "/")+"/", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := m.cli.RunAsAgent(ctx, iso.Container, "sh", "-c", dirsScript, "sh", parent)
	if err != nil {
		return api.Dirs{}, fmt.Errorf("listing %s: %w", parent, err)
	}
	ds := api.Dirs{Parent: parent, Dirs: []string{}, Home: m.home}
	for name := range strings.SplitSeq(out, "\n") {
		if name == "" || !strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) ||
			strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		ds.Dirs = append(ds.Dirs, parent+name+"/")
		if len(ds.Dirs) == dirsMax {
			break
		}
	}
	return ds, nil
}

// branchScript prints the branch of the repository $1 is in, or its
// commit when it is on none, and nothing when it is in none. It takes no
// lock, so it never stands in the way of the agent's own git.
const branchScript = `cd "$1" 2>/dev/null || exit 0
git --no-optional-locks symbolic-ref --short -q HEAD 2>/dev/null || git --no-optional-locks rev-parse --short -q HEAD 2>/dev/null || true`

// Branch is the git branch of the agent's workspace, for its status line:
// of where its CLI says it runs, a worktree perhaps, else its workdir.
func (m *Manager) Branch(ctx context.Context, id string) (api.Branch, error) {
	a, err := m.Agent(id)
	if err != nil {
		return api.Branch{}, err
	}
	dir := a.Workdir
	if u := a.State.Usage; u != nil && u.Cwd != "" {
		dir = u.Cwd
	}
	if a.ArchivedAt != nil {
		return api.Branch{}, nil
	}
	iso, err := m.store.Isolation(a.IsolationID)
	if err != nil {
		return api.Branch{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := m.cli.RunAsAgent(ctx, iso.Container, "sh", "-c", branchScript, "sh", dir)
	if err != nil {
		return api.Branch{}, fmt.Errorf("reading the workspace's branch: %w", err)
	}
	return api.Branch{Branch: strings.TrimSpace(out)}, nil
}

// filesMax is how many of a workspace's files are offered.
const filesMax = 20000

// filesScript lists the files under $1 by their paths there, each after
// "f ": git's own list in a repository, tracked and untracked but not
// ignored, every file otherwise. What git ignores comes after "i ", a
// folder of it by its name alone (ending in /), its files not listed: they
// are many at times (node_modules, a build's), and dirScript lists one's
// when asked. Out of a repository node_modules is such a folder.
var filesScript = `cd "$1" 2>/dev/null || exit 0
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	git -c core.quotepath=off ls-files -co --exclude-standard 2>/dev/null | sed 's/^/f /'
	git -c core.quotepath=off ls-files -oi --exclude-standard --directory 2>/dev/null | sed 's/^/i /'
else
	find . -name .git -prune -o -name node_modules -prune -exec printf 'i %s/\n' {} \; -o -type f -print 2>/dev/null | sed 's|^i \./|i |; s|^\./|f |'
fi | head -n ` + strconv.Itoa(filesMax+1)

// dirScript lists what is in the folder $2 of the workspace $1, by paths
// in the workspace, a folder's ending in /.
var dirScript = `cd "$1" 2>/dev/null && cd "./$2" 2>/dev/null || exit 0
for f in .[!.]* ..?* *; do
	if [ -d "$f" ] && [ ! -L "$f" ]; then echo "$2/$f/"
	elif [ -e "$f" ] || [ -L "$f" ]; then echo "$2/$f"; fi
done | head -n ` + strconv.Itoa(filesMax+1)

// Files are the files in the agent's workspace; with dir, what is in that
// folder of it (one git ignores, say). The isolation is not started for
// them: one that is stopped has none to offer.
func (m *Manager) Files(ctx context.Context, id, dir string) (api.Files, error) {
	if dir != "" {
		dir = path.Clean(dir)
		if path.IsAbs(dir) || dir == "." || dir == ".." || strings.HasPrefix(dir, "../") {
			return api.Files{}, badRequest(fmt.Errorf("%q is not a folder in the workspace", dir))
		}
	}
	a, err := m.storedAgent(id)
	if err != nil {
		return api.Files{}, err
	}
	if a.ArchivedAt != nil {
		return api.Files{}, badRequest(errors.New("an archived agent is sent nothing"))
	}
	iso, err := m.store.Isolation(a.IsolationID)
	if err != nil {
		return api.Files{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	argv := []string{"sh", "-c", filesScript, "sh", a.Workdir}
	if dir != "" {
		argv = []string{"sh", "-c", dirScript, "sh", a.Workdir, dir}
	}
	out, err := m.cli.RunAsAgent(ctx, iso.Container, argv...)
	if err != nil {
		return api.Files{}, fmt.Errorf("listing the workspace: %w", err)
	}
	fs := api.Files{Files: []string{}}
	n := 0
	for l := range strings.SplitSeq(out, "\n") {
		if l == "" {
			continue
		}
		if n++; n > filesMax {
			fs.Truncated = true
			break
		}
		list := &fs.Files
		if dir == "" {
			kind, p, _ := strings.Cut(l, " ")
			if kind == "i" {
				list = &fs.Ignored
			}
			l = p
		}
		// git quotes a name it cannot print as it is.
		if l != "" && !strings.HasPrefix(l, `"`) {
			*list = append(*list, l)
		}
	}
	return fs, nil
}
