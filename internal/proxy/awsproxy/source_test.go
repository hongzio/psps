package awsproxy

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newSource(t *testing.T, f *fakeAWS) *Source {
	t.Helper()
	p, err := SessionPolicy([]string{jp}, []string{bucket})
	if err != nil {
		t.Fatal(err)
	}
	return &Source{AWS: f.path, RoleARN: role, Profile: "dev", SessionName: "psps-axis", Policy: p.JSON()}
}

var epoch = time.Unix(0, 0)

func TestAssumeRoleIsInvokedWithTheScopingPolicy(t *testing.T) {
	f := newFakeAWS(t)
	if _, err := newSource(t, f).Get(context.Background(), epoch); err != nil {
		t.Fatal(err)
	}
	if a := f.argv(); !reflect.DeepEqual(a[:2], []string{"sts", "assume-role"}) {
		t.Fatalf("argv = %v", a)
	}
	if f.flag("--profile") != "dev" || f.flag("--role-arn") != role || f.flag("--role-session-name") != "psps-axis" {
		t.Fatalf("argv = %v", f.argv())
	}
	var sent Policy
	json.Unmarshal([]byte(f.flag("--policy")), &sent)
	want, _ := SessionPolicy([]string{jp}, []string{bucket})
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("policy sent = %s", f.flag("--policy"))
	}
}

func TestReturnsTheContainerCredentialWireShape(t *testing.T) {
	c, err := newSource(t, newFakeAWS(t)).Get(context.Background(), epoch)
	if err != nil {
		t.Fatal(err)
	}
	// The container-provider contract: Token, not SessionToken.
	b, _ := json.Marshal(c)
	var keys map[string]string
	json.Unmarshal(b, &keys)
	if len(keys) != 4 || keys["Token"] != "token" || keys["AccessKeyId"] != "ASIAEXAMPLE" || keys["SecretAccessKey"] == "" || keys["Expiration"] == "" {
		t.Fatalf("wire shape = %s", b)
	}
}

func TestCredentialsAreReusedUntilTheRefreshMargin(t *testing.T) {
	f := newFakeAWS(t)
	f.respond(cliPayload("ASIAEXAMPLE", "2026-01-01T01:00:00Z"), 0, "")
	expiry := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	start := expiry.Add(-time.Hour)
	src := newSource(t, f)
	ctx := context.Background()
	src.Get(ctx, start)
	src.Get(ctx, start.Add(2699*time.Second)) // the margin is 900s before expiry
	if f.calls() != 1 {
		t.Fatalf("assumed %d times inside the margin", f.calls())
	}
	src.Get(ctx, start.Add(2701*time.Second))
	if f.calls() != 2 {
		t.Fatalf("assumed %d times after the margin", f.calls())
	}
}

func TestAnExpiredHostSessionNamesTheOperatorAction(t *testing.T) {
	f := newFakeAWS(t)
	f.respond("", 255, "Error: Your session has expired. Please reauthenticate using 'aws login'.")
	_, err := newSource(t, f).Get(context.Background(), epoch)
	mustDeny(t, err, "aws login --profile dev")
}

func TestAnyOtherFailureCarriesTheStderr(t *testing.T) {
	f := newFakeAWS(t)
	f.respond("", 254, "AccessDenied: cannot assume")
	_, err := newSource(t, f).Get(context.Background(), epoch)
	mustDeny(t, err, "cannot assume")
}

func TestAFailureIsNotCached(t *testing.T) {
	f := newFakeAWS(t)
	f.respond("", 1, "transient")
	src := newSource(t, f)
	for range 2 {
		if _, err := src.Get(context.Background(), epoch); err == nil {
			t.Fatal("a failure issued credentials")
		}
	}
	if f.calls() != 2 {
		t.Fatalf("assumed %d times", f.calls())
	}
}

func TestOutputWithoutCredentialsIsRefused(t *testing.T) {
	f := newFakeAWS(t)
	f.respond(`{"AssumedRoleUser": {}}`, 0, "")
	_, err := newSource(t, f).Get(context.Background(), epoch)
	mustDeny(t, err, "returned no credentials")
}

func TestAMissingCLIIsNamed(t *testing.T) {
	src := newSource(t, newFakeAWS(t))
	src.AWS = "/nonexistent/aws"
	_, err := src.Get(context.Background(), epoch)
	mustDeny(t, err, "is not runnable")
	if !strings.Contains(err.Error(), "/nonexistent/aws") {
		t.Fatal(err)
	}
}
