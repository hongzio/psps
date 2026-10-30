package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
)

func commandNames(cs api.Commands) []string {
	var out []string
	for _, c := range cs.Commands {
		out = append(out, c.Prefix+c.Name)
	}
	return out
}

// gero's commands come first and are done, not sent; the agent's own come
// after, and /compact is codex compacting.
func TestCommands(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	a, _ := m.CreateAgent(context.Background(), api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "hi"})
	e.waitFor("the first turn", func() bool { return e.hasAssistant(a.ID, "echo: hi") })

	var cs api.Commands
	e.waitFor("the skills", func() bool {
		cs, _ = m.Commands(a.ID)
		return slices.Contains(commandNames(cs), "$tidy")
	})
	if got := strings.Join(commandNames(cs), " "); got != "/model /effort /approvals /stop /new /compact $tidy" {
		t.Fatal(got)
	}
	if !cs.Commands[0].Gero || !cs.Commands[4].Gero || cs.Commands[5].Gero {
		t.Fatalf("%+v", cs.Commands)
	}
	if args := cs.Commands[0].Args; len(args) != 2 || args[1].Value != "gpt-fake" || args[1].Description != "Fake · A fake" {
		t.Fatalf("models %+v", args)
	}
	if args := cs.Commands[1].Args; len(args) != 5 || args[4].Value != "xhigh" {
		t.Fatalf("efforts %+v", args)
	}

	for _, c := range []struct{ text, done string }{
		{"/model", "Model: the CLI's default."},
		{" /model gpt-fake ", "Model set to gpt-fake. Codex uses it from the next turn."},
		{"/model gpt-fake", "Model is already gpt-fake."},
		{"/effort high", "Effort set to high. Codex uses it from the next turn."},
		{"/approvals", "Approvals: never."},
		{"/approvals ask", "Approvals set to ask. Codex uses it from the next turn."},
		{"/model default", "Model set to the CLI's default. Codex uses it from the next turn."},
		{"/stop", "Nothing to stop: it is not working."},
	} {
		if sent, err := m.Message(a.ID, c.text); err != nil || sent.Done != c.done {
			t.Errorf("%q: %q %v", c.text, sent.Done, err)
		}
	}
	if v, _ := m.Agent(a.ID); v.Options != (driver.Options{Effort: "high", Approvals: driver.ApprovalsAsk}) {
		t.Fatalf("options %+v", v.Options)
	}
	for _, c := range []struct{ text, want string }{
		{"/effort max", `not "max"`},
		{"/approvals sometimes", "unknown approvals mode"},
		{"/model a b", "takes one value"},
		{"/stop now", "takes nothing"},
	} {
		var he httpError
		if _, err := m.Message(a.ID, c.text); !errors.As(err, &he) || he.code != 400 || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v", c.text, err)
		}
	}
	if got := users(e, a.ID); len(got) != 1 {
		t.Fatalf("gero's commands were sent: %q", got)
	}
	if err := m.Send(a.ID, "hang"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the hanging turn", func() bool { return status(m, a.ID) == event.StatusWorking })
	if sent, err := m.Message(a.ID, "/stop"); err != nil || sent.Done != "Stopping its turn." {
		t.Fatalf("%+v %v", sent, err)
	}
	e.waitFor("the turn to stop", func() bool { return status(m, a.ID) != event.StatusWorking })

	// Anything else is a message: a command of the agent's, or text that
	// only starts like one of gero's.
	for _, text := range []string{"/compact", "/models", "/model\nplease"} {
		if sent, err := m.Message(a.ID, text); err != nil || sent.Done != "" {
			t.Fatalf("%q: %+v %v", text, sent, err)
		}
	}
	e.waitFor("the messages", func() bool { return e.hasAssistant(a.ID, "echo: /model\nplease") })
	if got := users(e, a.ID); !slices.Equal(got, []string{"hi", "hang", "/compact", "/models", "/model\nplease"}) {
		t.Fatalf("turns %q", got)
	}
	var compacted bool
	for _, ev := range e.events(a.ID) {
		compacted = compacted || ev.Kind == event.KindNotice && ev.Text == "context compacted"
	}
	if !compacted || e.hasAssistant(a.ID, "echo: /compact") {
		t.Fatal("/compact was not codex compacting")
	}
}

func TestFiles(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	a, _ := m.CreateAgent(context.Background(), api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	write := func(rel string) {
		p := filepath.Join(a.Workdir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := func() []string {
		t.Helper()
		fs, err := m.Files(context.Background(), a.ID)
		if err != nil || fs.Truncated {
			t.Fatalf("%+v %v", fs, err)
		}
		slices.Sort(fs.Files)
		return fs.Files
	}
	if got := files(); len(got) != 0 {
		t.Fatalf("%q", got)
	}
	for _, f := range []string{"main.go", "src/a b.go", "node_modules/x/index.js", ".git/config"} {
		write(f)
	}
	if got := files(); !slices.Equal(got, []string{"main.go", "src/a b.go"}) {
		t.Fatalf("%q", got)
	}
	// In a repository, git's list: what it ignores is not offered.
	os.RemoveAll(filepath.Join(a.Workdir, ".git"))
	if out, err := exec.Command("git", "-C", a.Workdir, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git: %v %s", err, out)
	}
	write(".gitignore")
	os.WriteFile(filepath.Join(a.Workdir, ".gitignore"), []byte("build/\n"), 0o644)
	write("build/out.bin")
	if got := files(); !slices.Equal(got, []string{".gitignore", "main.go", "node_modules/x/index.js", "src/a b.go"}) {
		t.Fatalf("%q", got)
	}
}
