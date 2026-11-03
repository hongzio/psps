package ddproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/store"
)

// Ported from isolate's test_datadog_skill.py. The skill is the agent's
// operational truth and the only Datadog document it reads before deciding
// what it can do: if it said writes were refused everywhere while an
// operator had opened one family, the agent would decline work it was
// granted. So the per-env list carries the writes, and the refusal section
// points at the flags that widen them.

var skillEnvs = map[string]Env{
	"prod": {Site: "datadoghq.com", Families: []string{"monitors", "dashboards"}, Writes: []string{"dashboards"}, LogsQuery: "service:api"},
	"dev":  {Site: "datadoghq.eu", Families: []string{"monitors"}},
}

func skill(t *testing.T, expose []string, def string) string {
	t.Helper()
	v := proxy.View{Isolation: "axis", Enabled: true, Envs: map[string]json.RawMessage{}, Tokens: map[string]string{}}
	v.Exposure = store.Exposure{IsolationID: "axis", Service: "datadog", Envs: expose, Default: def}
	for _, n := range expose {
		b, _ := json.Marshal(skillEnvs[n])
		v.Envs[n] = b
		v.Tokens[n] = "tok-" + n
	}
	ws, err := Service{}.Workspace(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.Files) != 2 || ws.Files[0].Content != ws.Files[1].Content {
		t.Fatalf("the skill is not in both agents' homes: %+v", ws.Files)
	}
	if strings.Contains(ws.Files[0].Content, "%!") {
		t.Fatalf("a formatting slip:\n%s", ws.Files[0].Content)
	}
	return ws.Files[0].Content
}

func body(t *testing.T) string { return skill(t, []string{"prod", "dev"}, "prod") }

func TestAnEnvNamesItsFamiliesAndTheWritesAmongThem(t *testing.T) {
	if !strings.Contains(body(t), "`prod` (default) — api.datadoghq.com, families: monitors, dashboards; writes: dashboards") {
		t.Fatal(body(t))
	}
}

func TestAnEnvWithNoWritesReadsAsReadOnly(t *testing.T) {
	if !strings.Contains(body(t), "`dev` — api.datadoghq.eu, families: monitors; writes: none (read-only)") {
		t.Fatal(body(t))
	}
}

func TestTheOldBlanketRefusalIsGone(t *testing.T) {
	b := body(t)
	if strings.Contains(b, "refused for every environment") || strings.Contains(b, "read-only proxy") {
		t.Fatal(b)
	}
}

func TestTheRefusalSectionNamesTheWideningFlags(t *testing.T) {
	b := body(t)
	if !strings.Contains(b, "psps env edit datadog <env> --allow-write <family>") || !strings.Contains(b, "psps env edit datadog <env> --allow-api <family>") {
		t.Fatal(b)
	}
}

func TestTheWritesThatExistNowhereAreCalledOut(t *testing.T) {
	// mute_all and the write-surface-free families are refused even with
	// writes open; an agent that does not know reads those 404s as a bug.
	text := strings.Join(strings.Fields(body(t)), " ")
	if !strings.Contains(text, "mute_all") || !strings.Contains(text, "logs and traces have no write surface at all") {
		t.Fatal(text)
	}
}

func TestAnIsolationWhereNothingWritesStillExplainsTheShape(t *testing.T) {
	b := skill(t, []string{"dev"}, "dev")
	if !strings.Contains(b, "writes: none (read-only)") || !strings.Contains(b, "--allow-write <family>") {
		t.Fatal(b)
	}
}

func TestTheLogScopeOfTheDefaultEnvStillRides(t *testing.T) {
	if !strings.Contains(body(t), "`service:api`") {
		t.Fatal(body(t))
	}
	if strings.Contains(skill(t, []string{"dev"}, "dev"), "scope (`") {
		t.Fatal("a scope with none set")
	}
}

func TestNothingExposedSaysSoRatherThanRenderingAnEmptyList(t *testing.T) {
	if !strings.Contains(skill(t, nil, ""), "(none exposed yet)") {
		t.Fatal("empty list rendered")
	}
}

func TestTheWorkspaceCarriesTheKeysPerEnv(t *testing.T) {
	v := proxy.View{Isolation: "axis", Enabled: true, Envs: map[string]json.RawMessage{"prod": []byte(`{}`), "us-east": []byte(`{}`)},
		Tokens:   map[string]string{"prod": "t1", "us-east": "t2"},
		Exposure: store.Exposure{Envs: []string{"us-east", "prod"}, Default: "prod"}}
	ws, err := Service{}.Workspace(v)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"DD_API_KEY=t1", "DD_APP_KEY=t1", "DATADOG_API_KEY=t1", "DATADOG_APP_KEY=t1",
		"DATADOG_HOST=http://127.0.0.1:3023", "DD_API_KEY_PROD=t1", "DD_API_KEY_US_EAST=t2"}
	if strings.Join(ws.Env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%v", ws.Env)
	}
}
