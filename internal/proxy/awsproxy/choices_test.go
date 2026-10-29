package awsproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/proxy"
)

// inventoryAWS is an aws CLI that answers each subcommand from its own
// files, describe-instances per region: <dir>/<cmd>_<sub>[_<region>].out,
// .err and .code. Each call's argv is a line of <dir>/calls.
type inventoryAWS struct{ dir, path string }

func newInventoryAWS(t *testing.T) *inventoryAWS {
	t.Helper()
	f := &inventoryAWS{dir: t.TempDir()}
	f.path = filepath.Join(f.dir, "aws")
	script := fmt.Sprintf(`#!/bin/sh
d=%q
printf '%%s\n' "$*" >> "$d/calls"
region=""
prev=""
for a in "$@"; do
  [ "$prev" = --region ] && region=$a
  prev=$a
done
key="$1_$2"
[ "$2" = describe-instances ] && key="${key}_$region"
if [ -f "$d/noregion" ] && [ -z "$region" ]; then
  echo 'You must specify a region. You can also configure your region by running "aws configure".' >&2
  exit 253
fi
[ -f "$d/$key.err" ] && cat "$d/$key.err" >&2
[ -f "$d/$key.out" ] && cat "$d/$key.out"
exit $(cat "$d/$key.code" 2>/dev/null || echo 0)
`, f.dir)
	if err := os.WriteFile(f.path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *inventoryAWS) answer(key, stdout string, code int, stderr string) {
	os.WriteFile(filepath.Join(f.dir, key+".out"), []byte(stdout), 0o644)
	os.WriteFile(filepath.Join(f.dir, key+".code"), []byte(fmt.Sprint(code)), 0o644)
	os.WriteFile(filepath.Join(f.dir, key+".err"), []byte(stderr), 0o644)
}

func (f *inventoryAWS) calls() []string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "calls"))
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func (f *inventoryAWS) choices(t *testing.T, field string, env map[string]any) ([]proxy.Choice, error) {
	t.Helper()
	e := map[string]any{"aws_cli": f.path}
	for k, v := range env {
		e[k] = v
	}
	return Service{}.Choices(context.Background(), field, e, nil)
}

func spelled(cs []proxy.Choice) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Value+" ("+c.Note+")")
	}
	return strings.Join(out, "\n")
}

func TestTheProfilesAreTheMacsOwn(t *testing.T) {
	f := newInventoryAWS(t)
	f.answer("configure_list-profiles", "prod\ndev\n\n", 0, "")
	cs, err := f.choices(t, "aws_profile", map[string]any{"aws_profile": "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if got := spelled(cs); got != "dev ()\nprod ()" {
		t.Fatalf("profiles:\n%s", got)
	}
	// Listing the profiles is not done as one of them: the one typed may
	// not exist.
	if c := f.calls(); len(c) != 1 || c[0] != "configure list-profiles" {
		t.Fatalf("calls %q", c)
	}
}

func TestTheRolesAreTheOnesAPrincipalMayAssume(t *testing.T) {
	f := newInventoryAWS(t)
	f.answer("iam_list-roles", `{"Roles": [
	  {"Arn": "arn:aws:iam::111122223333:role/workspace", "Description": "for gero",
	   "AssumeRolePolicyDocument": {"Statement": [{"Effect": "Allow", "Action": "sts:AssumeRole",
	     "Principal": {"AWS": "arn:aws:iam::111122223333:root"}}]}},
	  {"Arn": "arn:aws:iam::111122223333:role/ci",
	   "AssumeRolePolicyDocument": {"Statement": {"Effect": "Allow", "Action": ["sts:AssumeRole", "sts:TagSession"],
	     "Principal": {"AWS": ["arn:aws:iam::444455556666:role/deployer", "777788889999"]}}}},
	  {"Arn": "arn:aws:iam::111122223333:role/encoded",
	   "AssumeRolePolicyDocument": "%7B%22Statement%22%3A%5B%7B%22Effect%22%3A%22Allow%22%2C%22Action%22%3A%22sts%3AAssumeRole%22%2C%22Principal%22%3A%7B%22AWS%22%3A%22arn%3Aaws%3Aiam%3A%3A111122223333%3Arole%2Fadmin%22%7D%7D%5D%7D"},
	  {"Arn": "arn:aws:iam::111122223333:role/service-role/lambda",
	   "AssumeRolePolicyDocument": {"Statement": [{"Effect": "Allow", "Action": "sts:AssumeRole",
	     "Principal": {"Service": "lambda.amazonaws.com"}}]}},
	  {"Arn": "arn:aws:iam::111122223333:role/aws-reserved/sso.amazonaws.com/AWSReservedSSO_Admin_0123",
	   "AssumeRolePolicyDocument": {"Statement": [{"Effect": "Allow", "Action": ["sts:AssumeRoleWithSAML", "sts:TagSession"],
	     "Principal": {"Federated": "arn:aws:iam::111122223333:saml-provider/AWSSSO_x_DO_NOT_DELETE"}}]}},
	  {"Arn": "arn:aws:iam::111122223333:role/denied",
	   "AssumeRolePolicyDocument": {"Statement": [{"Effect": "Deny", "Action": "sts:AssumeRole",
	     "Principal": {"AWS": "*"}}]}}
	]}`, 0, "")
	cs, err := f.choices(t, "role_arn", map[string]any{"aws_profile": "dev"})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"arn:aws:iam::111122223333:role/ci (trusts 444455556666:role/deployer, account 777788889999)",
		"arn:aws:iam::111122223333:role/encoded (trusts role/admin)",
		"arn:aws:iam::111122223333:role/workspace (trusts account 111122223333 · for gero)",
	}, "\n")
	if got := spelled(cs); got != want {
		t.Fatalf("roles:\n%s", got)
	}
	if c := f.calls(); c[0] != "iam list-roles --output json --profile dev" {
		t.Fatalf("calls %q", c)
	}
}

func TestARefusalBySSOIsNotALoginProblem(t *testing.T) {
	f := newInventoryAWS(t)
	f.answer("iam_list-roles", "", 254, "An error occurred (AccessDenied) when calling the ListRoles operation: User: arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_Dev_0123/me is not authorized to perform: iam:ListRoles")
	_, err := f.choices(t, "role_arn", map[string]any{"aws_profile": "dev"})
	mustDeny(t, err, "iam:ListRoles")
	if strings.Contains(err.Error(), "aws login") {
		t.Fatalf("an IAM refusal sent the operator to log in: %v", err)
	}
	f.answer("iam_list-roles", "", 255, "Error when retrieving token from sso: Token has expired and refresh failed")
	_, err = f.choices(t, "role_arn", map[string]any{"aws_profile": "dev"})
	mustDeny(t, err, "aws login --profile dev")
	f.answer("iam_list-roles", "", 255, "The config profile (dev) could not be found")
	_, err = f.choices(t, "role_arn", map[string]any{"aws_profile": "dev"})
	mustDeny(t, err, `no profile "dev"`)
}

func TestInstancesAreSweptFromEveryRegion(t *testing.T) {
	f := newInventoryAWS(t)
	f.answer("ec2_describe-regions", `["us-east-1", "ap-northeast-2", "eu-west-1"]`, 0, "")
	f.answer("ec2_describe-instances_ap-northeast-2", `[
	  ["i-0123456789abcdef0", "running", "bastion"],
	  ["i-0aaaaaaaaaaaaaaaa", "stopped", null],
	  ["i-0bbbbbbbbbbbbbbbb", "terminated", "old"]]`, 0, "")
	f.answer("ec2_describe-instances_us-east-1", `[["i-0ccccccccccccccc0", "running", "web"]]`, 0, "")
	// A region the account is kept out of is left out, not the answer.
	f.answer("ec2_describe-instances_eu-west-1", "", 254, "An error occurred (UnauthorizedOperation) when calling the DescribeInstances operation")
	// No region in the profile: describe-regions asks us-east-1.
	os.WriteFile(filepath.Join(f.dir, "noregion"), nil, 0o644)
	cs, err := f.choices(t, "instances", map[string]any{"aws_profile": "dev"})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"i-0123456789abcdef0 (bastion · ap-northeast-2)",
		"i-0aaaaaaaaaaaaaaaa (ap-northeast-2 · stopped)",
		"i-0ccccccccccccccc0 (web · us-east-1)",
	}, "\n")
	if got := spelled(cs); got != want {
		t.Fatalf("instances:\n%s", got)
	}
	calls := strings.Join(f.calls(), "\n")
	if !strings.Contains(calls, "ec2 describe-regions --query Regions[].RegionName --output json --region us-east-1 --profile dev") {
		t.Fatalf("calls:\n%s", calls)
	}

	// Every region refusing is the answer.
	for _, r := range []string{"us-east-1", "ap-northeast-2", "eu-west-1"} {
		f.answer("ec2_describe-instances_"+r, "", 255, "Error loading SSO Token: Token for dev has expired")
	}
	_, err = f.choices(t, "instances", map[string]any{"aws_profile": "dev"})
	mustDeny(t, err, "aws login --profile dev")
}

func TestBucketsAreTheAccountsWithTheirRegion(t *testing.T) {
	f := newInventoryAWS(t)
	f.answer("s3api_list-buckets", `[["transfer", "ap-northeast-2"], ["Logs-old", null]]`, 0, "")
	cs, err := f.choices(t, "buckets", map[string]any{"aws_profile": "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if got := spelled(cs); got != "Logs-old ()\ntransfer (ap-northeast-2)" {
		t.Fatalf("buckets:\n%s", got)
	}
}

func TestALookupNeedsAProfileAndTheCLI(t *testing.T) {
	f := newInventoryAWS(t)
	_, err := f.choices(t, "instances", nil)
	mustDeny(t, err, "pick an aws_profile first")
	var d *proxy.Denied
	if !errors.As(err, &d) {
		t.Fatalf("not a refusal: %v", err)
	}
	_, err = Service{}.Choices(context.Background(), "buckets", map[string]any{"aws_cli": "/nonexistent/aws", "aws_profile": "dev"}, nil)
	mustDeny(t, err, "install the AWS CLI")
}
