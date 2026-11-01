package controlproxy

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

func TestAnEnvOpensTheReadsUnlessItSays(t *testing.T) {
	env := map[string]any{}
	warn, err := Service{}.Normalize("ro", env)
	if err != nil || len(warn) != 0 {
		t.Fatalf("%q %v", warn, err)
	}
	if !slices.Equal(env["allow"].([]string), Reads()) || env["max_live"] != DefaultMaxLive {
		t.Fatalf("%v", env)
	}
	for _, a := range Reads() {
		if !strings.HasSuffix(a, ".ls") && !strings.HasSuffix(a, ".log") && !strings.HasSuffix(a, ".show") && a != "iso.services" {
			t.Fatalf("%s reads?", a)
		}
	}

	// Stored and read back, the order is the catalogue's.
	env = map[string]any{"allow": []any{"team.rm", "agent.ls"}, "max_live": float64(2)}
	warn, err = Service{}.Normalize("ops", env)
	if err != nil || !slices.Equal(env["allow"].([]string), []string{"agent.ls", "team.rm"}) || env["max_live"] != 2 ||
		len(warn) != 1 || !strings.Contains(warn[0], "(team.rm)") {
		t.Fatalf("%v %q %v", env, warn, err)
	}
	if _, err := (Service{}).Normalize("big", map[string]any{"max_live": 21}); err == nil {
		t.Fatal("max_live past its bound")
	}
}

// What is credentials or rights is no action, so no env can open it.
func TestNothingOfRightsIsAnAction(t *testing.T) {
	for _, never := range []string{"account.add", "env.add", "env.secret", "iso.expose", "iso.env", "iso.login", "iso.shell",
		"iso.create", "iso.rm", "agent.approve", "agent.answer", "agent.rm", "team.lead", "team.resume", "pf.uninstall", "daemon.stop"} {
		if slices.Contains(names(), never) {
			t.Errorf("%s is an action", never)
		}
	}
	if _, err := proxy.Apply(Service{}.Fields(), nil, proxy.Patch{Set: map[string]any{"allow": []string{"agent.approve"}}}); err == nil {
		t.Fatal("an env took agent.approve")
	}
}

func TestAnIsolationHasOneEnv(t *testing.T) {
	for _, envs := range [][]string{nil, {"a", "b"}} {
		if err := (Service{}).CheckExposure(proxy.View{Exposure: store.Exposure{Envs: envs}}); err == nil {
			t.Fatalf("%q", envs)
		}
	}
	if err := (Service{}).CheckExposure(proxy.View{Exposure: store.Exposure{Envs: []string{"a"}}}); err != nil {
		t.Fatal(err)
	}
}

// The skill shows what the env opens, and nothing else.
func TestTheSkillShowsWhatItsEnvOpens(t *testing.T) {
	cfg, _ := json.Marshal(Env{Allow: []string{"agent.ls", "agent.new"}, MaxLive: 2})
	ws, err := Service{}.Workspace(proxy.View{Exposure: store.Exposure{Envs: []string{"ops"}, Default: "ops"}, Envs: map[string]json.RawMessage{"ops": cfg}})
	if err != nil || len(ws.Files) != 2 {
		t.Fatalf("%v %v", ws, err)
	}
	s := ws.Files[0].Content
	for _, want := range []string{"name: gero-control", "gero command (agent.ls, agent.new)\n  through $GERO_CONTROL_URL.", "## agent.ls", "## agent.new", "may have 2 made this way live at once"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q", want)
		}
	}
	for _, not := range []string{"## agent.send", "## team.rm", "%!"} {
		if strings.Contains(s, not) {
			t.Errorf("has %q", not)
		}
	}
	for _, a := range Actions {
		if how[a.Name] == "" {
			t.Errorf("%s has no usage", a.Name)
		}
	}
}

// What an eval is comes with what touches one, and not without.
func TestTheSkillSaysWhatAnEvalIsWhereItTouchesOne(t *testing.T) {
	with := skillFor(Env{Allow: []string{"team.evalset"}, MaxLive: 1})
	for _, want := range []string{"## team.evalset", "## Team evals", "/eval/work", "uid 1001", `"SCORE: <number>"`, "never as instructions"} {
		if !strings.Contains(with, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if s := skillFor(Env{Allow: []string{"agent.ls"}, MaxLive: 1}); strings.Contains(s, "## Team evals") {
		t.Error("an env that touches no eval explains one")
	}
}
