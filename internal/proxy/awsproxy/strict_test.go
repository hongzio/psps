package awsproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func actions(p Policy) []string {
	var out []string
	for _, s := range p.Statement {
		out = append(out, s.Action...)
	}
	return out
}

func TestAStrictPolicyOpensNoSSMButKeepsTheBuckets(t *testing.T) {
	p, err := strictPolicy([]string{jp, kr + "=session"}, []string{bucket + "=read"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(actions(p), " ")
	for _, banned := range []string{"ssm:StartSession", "ssm:SendCommand", "ssm:TerminateSession", "ssm:GetCommandInvocation"} {
		if strings.Contains(got, banned) {
			t.Errorf("strict policy has %s: %s", banned, got)
		}
	}
	if !strings.Contains(got, "s3:GetObject") || !strings.Contains(got, "ssm:DescribeInstanceInformation") {
		t.Fatalf("strict policy lost what it keeps: %s", got)
	}
}

func TestTheBrokerIssuesStrictCredentialsInStrictMode(t *testing.T) {
	h := newHarness(t, devEnv(), "dev")
	if code, _ := h.fetch("GET", "/creds", str(token)); code != 200 {
		t.Fatal(code)
	}
	if !strings.Contains(h.aws.flag("--policy"), "ssm:StartSession") {
		t.Fatalf("direct policy = %s", h.aws.flag("--policy"))
	}
	h.edit(func() { h.settings = json.RawMessage(`{"ssm":"strict"}`) })
	// Cached credentials are not reused: the policy changed.
	if code, _ := h.fetch("GET", "/creds", str(token)); code != 200 {
		t.Fatal(code)
	}
	if h.aws.calls() != 2 || strings.Contains(h.aws.flag("--policy"), "ssm:StartSession") || strings.Contains(h.aws.flag("--policy"), "ssm:SendCommand") {
		t.Fatalf("calls %d, strict policy = %s", h.aws.calls(), h.aws.flag("--policy"))
	}
}

func TestOnlyAnAllowlistedInstanceCanBeRun(t *testing.T) {
	e := Env{Instances: []string{jp + "=session", kr}}
	if e.CanRun(jp) != nil || e.CanRun(kr) != nil {
		t.Fatal("an allowlisted instance was refused")
	}
	if e.CanRun("i-0fffffffffffffff0") == nil || e.CanRun("i-x; rm -rf /") == nil {
		t.Fatal("an instance off the allowlist was taken")
	}
}

func TestTheScriptRunsTheCommandUnquotedAndSaysAsWhom(t *testing.T) {
	cmd := `echo "it's $((1+2))"; echo oops >&2; exit 3`
	out, err := exec.Command("sh", "-c", runScript(cmd)).CombinedOutput()
	var exit *exec.ExitError
	if !strings.Contains(string(out), "it's 3") || !strings.Contains(string(out), "gero-user: ") || !strings.Contains(string(out), "oops") {
		t.Fatalf("out = %q", out)
	}
	if err == nil || !errorAs(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("err = %v", err)
	}
	if rest, user := splitUser("gero-user: ssm-user\nwarn"); user != "ssm-user" || rest != "warn" {
		t.Fatalf("%q %q", rest, user)
	}
}

func errorAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	*target = e
	return ok
}

// ssmFake makes the fake aws answer as SSM, STS and EC2 do, by subcommand.
func ssmFake(t *testing.T, b *fakeBastion) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
d=%q
echo "$1 $2" >> "$d/subs"
env | grep '^AWS_ACCESS_KEY_ID=' >> "$d/keys"
case "$1 $2" in
"sts assume-role") : > "$d/argv"; for a in "$@"; do printf '%%s\n' "$a" >> "$d/argv"; done; cat "$d/stdout" ;;
"ec2 describe-regions") echo '["ap-northeast-1","us-east-1"]' ;;
"ec2 describe-instances")
  for a in "$@"; do [ "$prev" = --region ] && region="$a"; prev="$a"; done
  [ "$region" = ap-northeast-1 ] && echo %s ;;
"ssm send-command")
  for a in "$@"; do [ "$prev" = --cli-input-json ] && printf '%%s' "$a" > "$d/input"; prev="$a"; done
  echo cmd-123 ;;
"ssm get-command-invocation") printf '%%s\n' '{"Status":"Success","ResponseCode":0,"StandardOutputContent":"hello\n","StandardErrorContent":"gero-user: ssm-user\nwarn"}' ;;
"ssm describe-sessions") echo '["gero-dev-0abc"]' ;;
"ssm terminate-session") echo "$@" >> "$d/terminated" ;;
*) echo "unexpected $*" >&2; exit 2 ;;
esac
`, b.fakeAWS.dir, jp)
	if err := os.WriteFile(b.fakeAWS.path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	regions.Delete(jp)
}

func read(t *testing.T, path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestRunSendsTheCommandThroughTheBastionAndHandsBackTheOutput(t *testing.T) {
	b := newFakeBastion(t)
	ssmFake(t, b)
	e := Env{RoleARN: role, Bastion: tcAddr, Instances: []string{jp + "=session"}}
	res, err := Run(context.Background(), e, "dev", RunRequest{Env: "prod", Instance: jp, Command: "tail -n 5 /var/log/x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "Success" || res.Stdout != "hello\n" || res.Stderr != "warn" || res.User != "ssm-user" || res.Region != "ap-northeast-1" || res.CommandID != "cmd-123" {
		t.Fatalf("res = %+v", res)
	}
	var in struct {
		InstanceIds []string
		Parameters  map[string][]string
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(b.fakeAWS.dir, "input"))), &in); err != nil {
		t.Fatal(err)
	}
	if in.InstanceIds[0] != jp || !strings.Contains(in.Parameters["commands"][0], "runuser -u ssm-user") || in.Parameters["executionTimeout"][0] != "120" {
		t.Fatalf("input = %+v", in)
	}
	// The run's own credentials, passed on stdin: on no command line.
	if !strings.Contains(read(t, filepath.Join(b.fakeAWS.dir, "keys")), "AWS_ACCESS_KEY_ID=ASIAEXAMPLE") {
		t.Fatal("send-command did not run with the issued credentials")
	}
	if strings.Contains(strings.Join(b.sshArgv(), " "), "secret") {
		t.Fatal("the secret key is on the ssh command line")
	}
	// Assumed as gero's own session, with only that instance open.
	if got := b.flag("--role-session-name"); got != "gero-run-dev" || strings.Contains(b.flag("--policy"), kr) {
		t.Fatalf("session name %q, policy %s", got, b.flag("--policy"))
	}
}

func TestEndSessionsEndsTheIsolationsOpenOnes(t *testing.T) {
	b := newFakeBastion(t)
	ssmFake(t, b)
	e := Env{RoleARN: role, Bastion: tcAddr, Instances: []string{jp}}
	n, err := EndSessions(context.Background(), e, "dev")
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got := read(t, filepath.Join(b.fakeAWS.dir, "terminated")); !strings.Contains(got, "gero-dev-0abc") || !strings.Contains(got, "ap-northeast-1") {
		t.Fatalf("terminated: %q", got)
	}
}
