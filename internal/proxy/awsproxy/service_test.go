package awsproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

func normalize(t *testing.T, env map[string]any) ([]string, error) {
	t.Helper()
	return Service{}.Normalize("dev", env)
}

func TestNormalizeCanonicalisesAndPinsTheCLI(t *testing.T) {
	f := newFakeAWS(t)
	env := map[string]any{"role_arn": role, "aws_profile": "dev", "aws_cli": f.path,
		"instances": []any{jp + "=session", jp + "=command", kr + "=command"}, "buckets": []string{bucket + "=write,read"}}
	warn, err := normalize(t, env)
	if err != nil || len(warn) != 0 {
		t.Fatalf("%v %v", warn, err)
	}
	if !reflect.DeepEqual(env["instances"], []string{jp, kr + "=command"}) || !reflect.DeepEqual(env["buckets"], []string{bucket + "=read,write"}) {
		t.Fatalf("env = %v", env)
	}
	if env["aws_cli"] != f.path {
		t.Fatalf("aws_cli = %v", env["aws_cli"])
	}
}

func TestNormalizeRefusesWhatCannotWork(t *testing.T) {
	cli := newFakeAWS(t).path
	for want, env := range map[string]map[string]any{
		"--role-arn is required":                   {"aws_profile": "dev", "aws_cli": cli},
		"does not look like":                       {"role_arn": "role/x", "aws_profile": "dev", "aws_cli": cli},
		"--aws-profile (or --bastion) is required": {"role_arn": role, "aws_cli": cli},
		"--aws-cli":                                {"role_arn": role, "aws_profile": "dev", "aws_cli": "/nonexistent/aws"},
		"unknown capability":                       {"role_arn": role, "aws_profile": "dev", "aws_cli": cli, "instances": []string{jp + "=shell"}},
		"STS caps it at 2048":                      {"role_arn": role, "aws_profile": "dev", "aws_cli": cli, "instances": manyInstances(80)},
	} {
		_, err := normalize(t, env)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}

func manyInstances(n int) []string {
	var out []string
	for i := range n {
		out = append(out, fmt.Sprintf("i-%017d", i))
	}
	return out
}

func TestAnEmptyAllowlistIsLegalButWarned(t *testing.T) {
	warn, err := normalize(t, map[string]any{"role_arn": role, "aws_profile": "dev", "aws_cli": newFakeAWS(t).path})
	if err != nil || len(warn) != 1 || !strings.Contains(warn[0], "empty allowlist") {
		t.Fatalf("%v %v", warn, err)
	}
}

func view(envs map[string]Env, def string) proxy.View {
	v := proxy.View{Isolation: "dev", Enabled: true, Envs: map[string]json.RawMessage{}, Tokens: map[string]string{"": "tok"}}
	v.Exposure = store.Exposure{IsolationID: "dev", Service: "aws", Default: def}
	for n, e := range envs {
		b, _ := json.Marshal(e)
		v.Envs[n] = b
		v.Exposure.Envs = append(v.Exposure.Envs, n)
	}
	return v
}

func TestTheWorkspaceGetsTheContainerVariablesAndTheSkill(t *testing.T) {
	envs := devEnv()
	envs["prod"] = Env{RoleARN: role, Profile: "prod", Instances: []string{kr + "=session"}}
	ws, err := Service{}.Workspace(view(envs, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ws.Env, []string{"AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:3021/creds", "AWS_CONTAINER_AUTHORIZATION_TOKEN=tok"}) {
		t.Fatalf("env = %v", ws.Env)
	}
	if len(ws.Files) != 2 {
		t.Fatalf("files = %+v", ws.Files)
	}
	skill := ws.Files[0].Content
	for _, want := range []string{"name: gero-aws", "- `dev` — instances: " + jp, "- `prod` — instances: " + kr + "=session; buckets: (none)",
		"is\n`dev`", "127.0.0.1:3021/creds/<env>", "gero env edit aws <env>"} {
		if !strings.Contains(skill, want) {
			t.Errorf("the skill lacks %q", want)
		}
	}
	if strings.Contains(skill, "%!") {
		t.Fatalf("a formatting slip in the skill:\n%s", skill)
	}
}

func TestCheckAssumesTheRoleAndAsksWhoItIs(t *testing.T) {
	f := newFakeAWS(t)
	t.Setenv("AWS_PROFILE", "admin")
	cfg, _ := json.Marshal(Env{RoleARN: role, Profile: "dev", Instances: []string{jp}, CLI: f.path})
	out, err := Service{}.Check(context.Background(), "dev", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	// get-caller-identity ran on the issued session, not the Mac's profile.
	if !strings.Contains(out, "identity   arn:aws:sts::050017197842:assumed-role/axis-tre-workspace/token") || !strings.Contains(out, "instances  "+jp) {
		t.Fatalf("out = %s", out)
	}
	if strings.Contains(f.env(), "AWS_PROFILE=") {
		t.Fatal("the host profile leaked into the identity check")
	}
	if got := f.flag("--output"); got != "json" {
		t.Fatalf("argv = %v", f.argv())
	}
}
