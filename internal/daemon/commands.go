package daemon

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
)

// Commands, for a message box to offer as they are typed: gero's own, which
// a message that is one of runs instead of being sent, then those of the
// agent's CLI, as its process last told its driver. Files are the
// workspace's, for a message to name one with @.

// geroCommands are the ones gero runs itself: the agent's settings, which
// are gero's to keep (what its CLI changes of them lasts only until gero
// next starts it), and stopping its turn. models are the ones its CLI
// said it runs.
func geroCommands(kind driver.Kind, models []driver.Choice) []api.Command {
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
		return api.Command{Command: driver.Command{Prefix: "/", Name: name, Description: desc, Hint: hint, Args: args}, Gero: true}
	}
	return []api.Command{
		cmd("model", "Set the model gero runs it with", "<model>", ms),
		cmd("effort", "Set how hard it thinks", "<level>", efforts),
		cmd("approvals", "Set what it asks you first", "<mode>", []driver.Choice{
			{Value: "never", Description: "run unattended"},
			{Value: "auto", Description: "a reviewer model refuses risky actions"},
			{Value: "ask", Description: "it asks first"},
			{Value: "default", Description: "the CLI's own settings decide"},
		}),
		cmd("stop", "Stop its turn", "", nil),
	}
}

func (m *Manager) Commands(id string) (api.Commands, error) {
	r, err := m.runtime(id)
	if err != nil {
		return api.Commands{}, err
	}
	kind, cmds, models := r.commands()
	out := geroCommands(kind, models)
	for _, c := range cmds {
		if !slices.ContainsFunc(out, func(g api.Command) bool { return g.Prefix == c.Prefix && g.Name == c.Name }) {
			out = append(out, api.Command{Command: c})
		}
	}
	return api.Commands{Commands: out}, nil
}

func (r *runtime) commands() (driver.Kind, []driver.Command, []driver.Choice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agent.Kind, r.drv.Commands(), r.drv.Models()
}

// geroCommand reads a message that is one of gero's commands: one line, the
// command and what it takes.
func geroCommand(text string) (name string, args []string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") || strings.Contains(t, "\n") {
		return "", nil, false
	}
	f := strings.Fields(t)
	switch name = f[0][1:]; name {
	case "model", "effort", "approvals", "stop":
		return name, f[1:], true
	}
	return "", nil, false
}

// Message sends text to the agent, or does what it says instead when it is
// one of gero's commands.
func (m *Manager) Message(id, text string) (api.Sent, error) {
	name, args, ok := geroCommand(text)
	if !ok {
		return api.Sent{}, m.Send(id, text)
	}
	r, err := m.runtime(id)
	if err != nil {
		return api.Sent{}, err
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
	*field = v
	if err := m.SetOptions(a.ID, o); err != nil {
		return api.Sent{}, err
	}
	when := "Codex uses it from the next turn."
	if a.Kind == driver.Claude {
		when = "Claude restarts with it when it is idle."
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

// filesMax is how many of a workspace's files are offered.
const filesMax = 20000

// filesScript lists the files under $1 by their paths there: git's own
// list in a repository, tracked and untracked but not ignored, every file
// otherwise.
var filesScript = `cd "$1" 2>/dev/null || exit 0
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	git -c core.quotepath=off ls-files -co --exclude-standard 2>/dev/null
else
	find . \( -name .git -o -name node_modules \) -prune -o -type f -print 2>/dev/null | sed 's|^\./||'
fi | head -n ` + strconv.Itoa(filesMax+1)

// Files are the files in the agent's workspace. The isolation is not
// started for them: one that is stopped has none to offer.
func (m *Manager) Files(ctx context.Context, id string) (api.Files, error) {
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
	out, err := m.cli.RunAsAgent(ctx, iso.Container, "sh", "-c", filesScript, "sh", a.Workdir)
	if err != nil {
		return api.Files{}, fmt.Errorf("listing the workspace: %w", err)
	}
	fs := api.Files{Files: []string{}}
	for l := range strings.SplitSeq(out, "\n") {
		// git quotes a name it cannot print as it is.
		if l != "" && !strings.HasPrefix(l, `"`) {
			fs.Files = append(fs.Files, l)
		}
	}
	if len(fs.Files) > filesMax {
		fs.Files, fs.Truncated = fs.Files[:filesMax], true
	}
	return fs, nil
}
