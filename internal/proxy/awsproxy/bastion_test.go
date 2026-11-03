package awsproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/creds"
)

const tcAddr = "tc1.example-bastion-address"

// fakeBastion is ssh and tailcat on disk: ssh records its argv, then runs
// the remote command line with a real sh, where the fake aws is on PATH,
// as the bastion's shell would. So quoting is tested end to end.
type fakeBastion struct {
	*fakeAWS
	dir, tailcat string
}

func newFakeBastion(t *testing.T) *fakeBastion {
	t.Helper()
	b := &fakeBastion{fakeAWS: newFakeAWS(t), dir: t.TempDir()}
	b.tailcat = filepath.Join(b.dir, "tailcat")
	os.WriteFile(b.tailcat, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	ssh := filepath.Join(b.dir, "ssh")
	os.WriteFile(ssh, []byte(fmt.Sprintf(`#!/bin/sh
d=%q
: > "$d/sshargv"
for a in "$@"; do printf '%%s\n' "$a" >> "$d/sshargv"; done
for a in "$@"; do
  case "$a" in
  -O) [ -f "$d/master" ] && exit 0; exit 255 ;;
  -N) [ -f "$d/sshcode" ] && exit 255; echo x >> "$d/master"; exit 0 ;;
  esac
done
if [ -f "$d/sshcode" ]; then echo "ssh: connect to host bastion: Connection timed out" >&2; exit $(cat "$d/sshcode"); fi
for last; do :; done
exec sh -c "$last"
`, b.dir)), 0o755)
	t.Setenv("PATH", b.fakeAWS.dir+":"+b.dir+":/usr/bin:/bin")
	old, oldKH, oldCP := sshBin, knownHosts, controlPath
	sshBin = ssh
	kh := filepath.Join(t.TempDir(), "known_hosts")
	knownHosts = func() string { return kh }
	controlPath = func() string { return filepath.Join(b.dir, "cm-%C") }
	t.Cleanup(func() { sshBin, knownHosts, controlPath = old, oldKH, oldCP })
	return b
}

func (b *fakeBastion) sshArgv() []string {
	out, _ := os.ReadFile(filepath.Join(b.dir, "sshargv"))
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

// masters is how many ssh masters were started.
func (b *fakeBastion) masters() int {
	out, _ := os.ReadFile(filepath.Join(b.dir, "master"))
	return strings.Count(string(out), "x")
}

func (b *fakeBastion) unreachable() {
	os.WriteFile(filepath.Join(b.dir, "sshcode"), []byte("255"), 0o644)
}

func TestThroughABastionTheRoleIsAssumedThereWithThePolicyIntact(t *testing.T) {
	b := newFakeBastion(t)
	p, _ := SessionPolicy([]string{jp}, []string{bucket})
	src := &Source{Bastion: tcAddr, Profile: "ignored", RoleARN: role, SessionName: "psps-it's", Policy: p.JSON()}
	c, err := src.Get(context.Background(), epoch)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyId != "ASIAEXAMPLE" || src.Assumed() == "" {
		t.Fatalf("creds = %+v, assumed %q", c, src.Assumed())
	}
	// The bastion's aws got the argv as the Mac's would have, policy JSON
	// and quote included, and no profile: its instance role is the login.
	if !reflect.DeepEqual(b.argv()[:2], []string{"sts", "assume-role"}) || b.flag("--policy") != p.JSON() ||
		b.flag("--role-session-name") != "psps-it's" || slices.Contains(b.argv(), "--profile") {
		t.Fatalf("aws argv = %q", b.argv())
	}
	ssh := strings.Join(b.sshArgv(), "\n")
	for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=accept-new",
		"ProxyCommand='" + b.tailcat + "' '" + tcAddr + "' 22", "UserKnownHostsFile=", "ControlMaster=no", "ControlPath="} {
		if !strings.Contains(ssh, want) {
			t.Errorf("ssh argv lacks %q:\n%s", want, ssh)
		}
	}
}

func TestABastionThatDoesNotAnswerIsSaidToBeThatNotAnExpiredLogin(t *testing.T) {
	b := newFakeBastion(t)
	b.unreachable()
	_, err := (&Source{Bastion: tcAddr, RoleARN: role, SessionName: "s", Policy: "{}"}).Get(context.Background(), epoch)
	if err == nil || !strings.Contains(err.Error(), "did not answer") || !strings.Contains(err.Error(), "--allow") ||
		strings.Contains(err.Error(), "aws login") {
		t.Fatalf("err = %v", err)
	}
	if b.calls() != 0 {
		t.Fatal("aws ran though ssh did not connect")
	}
}

func TestAnAWSRefusalThroughABastionIsPassedOnAsAWSSpelledIt(t *testing.T) {
	b := newFakeBastion(t)
	b.respond("", 254, "An error occurred (AccessDenied) when calling the AssumeRole operation: not authorized; SSO session expired")
	_, err := (&Source{Bastion: tcAddr, RoleARN: role, SessionName: "s", Policy: "{}"}).Get(context.Background(), epoch)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || strings.Contains(err.Error(), "aws login") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnEnvWithABastionIsSavedOnlyOnceTheBastionAnswers(t *testing.T) {
	newFakeBastion(t)
	asked := ""
	old := checkBastion
	t.Cleanup(func() { checkBastion = old })
	checkBastion = func(addr string) error { asked = addr; return nil }

	// No profile and no aws on the Mac are needed.
	env := map[string]any{"role_arn": role, "bastion": " " + tcAddr + " ", "aws_cli": "/nonexistent/aws", "buckets": []string{bucket}}
	if _, err := normalize(t, env); err != nil {
		t.Fatal(err)
	}
	if asked != tcAddr || env["bastion"] != tcAddr {
		t.Fatalf("asked %q, env %v", asked, env)
	}

	checkBastion = func(string) error { return errors.New("the bastion did not answer") }
	if _, err := normalize(t, map[string]any{"role_arn": role, "bastion": tcAddr}); err == nil || !strings.Contains(err.Error(), "not saved") {
		t.Fatalf("err = %v", err)
	}
	if _, err := normalize(t, map[string]any{"role_arn": role, "bastion": "two words"}); err == nil {
		t.Fatal("a bastion with a space was taken")
	}
}

func TestAnEnvWithABastionIsRefusedWithoutTailcatOnTheMac(t *testing.T) {
	newFakeBastion(t)
	t.Setenv("PATH", t.TempDir())
	oldDirs, oldCheck := tailcatDirs, checkBastion
	tailcatDirs = nil
	checkBastion = func(string) error { t.Fatal("asked the bastion without tailcat"); return nil }
	t.Cleanup(func() { tailcatDirs, checkBastion = oldDirs, oldCheck })
	_, err := normalize(t, map[string]any{"role_arn": role, "bastion": tcAddr})
	if err == nil || !strings.Contains(err.Error(), "brew install tailcat") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckThroughABastionNamesTheBastionAndTheAssumedRole(t *testing.T) {
	newFakeBastion(t)
	cfg, _ := json.Marshal(Env{RoleARN: role, Bastion: tcAddr, Buckets: []string{bucket}})
	out, err := Service{}.Check(context.Background(), "dev", cfg, (*creds.Store)(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bastion    arn:aws:sts::") || !strings.Contains(out, "identity   arn:aws:sts::050017197842:assumed-role/axis-tre-workspace/psps-dev") {
		t.Fatalf("out = %s", out)
	}
}

func TestLookupsGoThroughTheBastionWithNoProfile(t *testing.T) {
	b := newFakeBastion(t)
	b.respond(`{"Roles": []}`, 0, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := (Service{}).Choices(ctx, "role_arn", map[string]any{"bastion": tcAddr}, nil); err != nil {
		t.Fatal(err)
	}
	if a := b.argv(); a[0] != "iam" || slices.Contains(a, "--profile") {
		t.Fatalf("aws argv = %q", a)
	}
}

func TestCommandsToABastionShareOneMaster(t *testing.T) {
	b := newFakeBastion(t)
	ctx := context.Background()
	done := make(chan error)
	for range 8 {
		go func() {
			_, err := CLI{Bastion: tcAddr}.Identity(ctx)
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// One tailcat for the eight: two clients with the Mac's key at once
	// push each other off.
	if b.masters() != 1 {
		t.Fatalf("%d masters started", b.masters())
	}
}
