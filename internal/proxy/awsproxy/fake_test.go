package awsproxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAWS is an aws CLI on disk, so the broker's real exec path runs: it
// records each call's argv and environment, and answers assume-role with
// whatever the test set. get-caller-identity names the session token it
// was handed, so a test can tell the issued credentials were the ones used.
type fakeAWS struct {
	dir, path string
}

func newFakeAWS(t *testing.T) *fakeAWS {
	t.Helper()
	f := &fakeAWS{dir: t.TempDir()}
	f.path = filepath.Join(f.dir, "aws")
	script := fmt.Sprintf(`#!/bin/sh
d=%q
echo x >> "$d/calls"
: > "$d/argv"
for a in "$@"; do printf '%%s\n' "$a" >> "$d/argv"; done
env > "$d/env"
if [ "$2" = get-caller-identity ]; then
  printf '{"Arn": "arn:aws:sts::050017197842:assumed-role/axis-tre-workspace/%%s"}\n' "$AWS_SESSION_TOKEN"
  exit 0
fi
[ -f "$d/stderr" ] && cat "$d/stderr" >&2
[ -f "$d/stdout" ] && cat "$d/stdout"
exit $(cat "$d/code" 2>/dev/null || echo 0)
`, f.dir)
	if err := os.WriteFile(f.path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Valid for an hour from now, as STS would issue them.
	f.respond(cliPayload("ASIAEXAMPLE", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)), 0, "")
	return f
}

func (f *fakeAWS) respond(stdout string, code int, stderr string) {
	os.WriteFile(filepath.Join(f.dir, "stdout"), []byte(stdout), 0o644)
	os.WriteFile(filepath.Join(f.dir, "code"), []byte(fmt.Sprint(code)), 0o644)
	os.WriteFile(filepath.Join(f.dir, "stderr"), []byte(stderr), 0o644)
}

func (f *fakeAWS) calls() int {
	b, _ := os.ReadFile(filepath.Join(f.dir, "calls"))
	return strings.Count(string(b), "x")
}

func (f *fakeAWS) argv() []string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "argv"))
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func (f *fakeAWS) env() string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "env"))
	return string(b)
}

// flag is the value after name in the last call's argv.
func (f *fakeAWS) flag(name string) string {
	a := f.argv()
	for i := range a[:len(a)-1] {
		if a[i] == name {
			return a[i+1]
		}
	}
	return ""
}

const role = "arn:aws:iam::050017197842:role/axis-tre-workspace"

func cliPayload(keyID, expiration string) string {
	b, _ := json.Marshal(map[string]any{
		"Credentials": map[string]string{
			"AccessKeyId": keyID, "SecretAccessKey": "secret", "SessionToken": "token", "Expiration": expiration,
		},
		"AssumedRoleUser": map[string]string{"Arn": "arn:aws:sts::050017197842:assumed-role/axis-tre-workspace/psps-dev"},
	})
	return string(b)
}
