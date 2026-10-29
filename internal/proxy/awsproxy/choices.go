package awsproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/proxy"
)

// Choices is what an aws env's fields can take, as the Mac's AWS CLI sees
// it: its profiles, and with the env's profile the roles that account has
// for a person to assume, its instances in every enabled region and its
// buckets. The profile's login is the credential; nothing is stored.
func (Service) Choices(ctx context.Context, field string, env map[string]any, _ map[string]string) ([]proxy.Choice, error) {
	cli := strings.TrimSpace(proxy.StringOr(env, "aws_cli", ""))
	if cli == "" {
		cli = "aws"
	}
	path, err := exec.LookPath(cli)
	if err != nil {
		return nil, proxy.Deny("%q is not on the Mac's PATH; install the AWS CLI or give aws_cli its full path", cli)
	}
	if field == "aws_profile" {
		return runner{cli: path}.profiles(ctx)
	}
	r := runner{cli: path, profile: strings.TrimSpace(proxy.StringOr(env, "aws_profile", ""))}
	if r.profile == "" {
		return nil, proxy.Deny("pick an aws_profile first: what is listed is what its login sees")
	}
	switch field {
	case "role_arn":
		return r.roles(ctx)
	case "instances":
		return r.instances(ctx)
	case "buckets":
		return r.buckets(ctx)
	}
	return nil, fmt.Errorf("aws has no choices for %s", field)
}

type runner struct {
	cli, profile string
}

// run is one aws command with the profile, its JSON answer or why not. A
// profile with no region set gets us-east-1, which answers for the whole
// account: describe-regions, IAM and the bucket list are not regional.
func (r runner) run(ctx context.Context, what string, args ...string) ([]byte, error) {
	out, err := r.call(ctx, what, args)
	var d *proxy.Denied
	if errors.As(err, &d) && strings.Contains(d.Reason, "must specify a region") {
		out, err = r.call(ctx, what, append(args[:len(args):len(args)], "--region", "us-east-1"))
	}
	return out, err
}

func (r runner) call(ctx context.Context, what string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	argv := append([]string(nil), args...)
	if r.profile != "" {
		argv = append(argv, "--profile", r.profile)
	}
	cmd := exec.CommandContext(ctx, r.cli, argv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, proxy.Deny("%s timed out; is the network up?", what)
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return nil, proxy.Deny("aws CLI %q is not runnable: %v", r.cli, err)
	}
	if err != nil {
		return nil, refusal(what, r.profile, stderr.String())
	}
	return out, nil
}

func (r runner) profiles(ctx context.Context) ([]proxy.Choice, error) {
	out, err := r.run(ctx, "aws configure list-profiles", "configure", "list-profiles")
	if err != nil {
		return nil, err
	}
	var cs []proxy.Choice
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			cs = append(cs, proxy.Choice{Value: l})
		}
	}
	sortChoices(cs)
	return cs, nil
}

// roles are the account's roles an AWS principal may assume, as the
// broker's profile must: one only a service or an identity provider may
// assume (a service role, the SSO permission sets' own) is left out. A
// role in another account is not listed, and is typed.
func (r runner) roles(ctx context.Context) ([]proxy.Choice, error) {
	out, err := r.run(ctx, "iam list-roles", "iam", "list-roles", "--output", "json")
	if err != nil {
		return nil, err
	}
	var got struct {
		Roles []struct {
			Arn, Description         string
			AssumeRolePolicyDocument json.RawMessage
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		return nil, fmt.Errorf("unexpected answer from iam list-roles: %w", err)
	}
	var cs []proxy.Choice
	for _, role := range got.Roles {
		who := trustedBy(role.AssumeRolePolicyDocument)
		if len(who) == 0 {
			continue
		}
		note := "trusts " + strings.Join(short(who, accountOf(role.Arn)), ", ")
		if d := strings.TrimSpace(role.Description); d != "" {
			if len(d) > 80 {
				d = d[:77] + "..."
			}
			note += " · " + d
		}
		cs = append(cs, proxy.Choice{Value: role.Arn, Note: note})
	}
	sortChoices(cs)
	return cs, nil
}

// trustedBy is the AWS principals a trust policy lets assume the role:
// accounts, roles, users, or "*". The CLI hands the document over decoded;
// the API's URL-encoded string is read too.
func trustedBy(doc json.RawMessage) []string {
	var s string
	if json.Unmarshal(doc, &s) == nil {
		if dec, err := url.QueryUnescape(s); err == nil {
			s = dec
		}
		doc = json.RawMessage(s)
	}
	var policy struct {
		Statement json.RawMessage
	}
	if json.Unmarshal(doc, &policy) != nil {
		return nil
	}
	var stmts []struct {
		Effect    string
		Action    json.RawMessage
		Principal json.RawMessage
	}
	if json.Unmarshal(policy.Statement, &stmts) != nil {
		var one struct {
			Effect    string
			Action    json.RawMessage
			Principal json.RawMessage
		}
		if json.Unmarshal(policy.Statement, &one) != nil {
			return nil
		}
		stmts = append(stmts, one)
	}
	seen := map[string]bool{}
	var who []string
	for _, st := range stmts {
		if st.Effect != "Allow" || !assumes(oneOrMany(st.Action)) {
			continue
		}
		var principals []string
		var star string
		if json.Unmarshal(st.Principal, &star) == nil && star == "*" {
			principals = []string{"*"}
		} else {
			var p struct{ AWS json.RawMessage }
			if json.Unmarshal(st.Principal, &p) == nil {
				principals = oneOrMany(p.AWS)
			}
		}
		for _, x := range principals {
			if !seen[x] {
				seen[x] = true
				who = append(who, x)
			}
		}
	}
	return who
}

func assumes(actions []string) bool {
	for _, a := range actions {
		switch strings.ToLower(a) {
		case "sts:assumerole", "sts:*", "*":
			return true
		}
	}
	return false
}

func oneOrMany(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	json.Unmarshal(raw, &many)
	return many
}

func accountOf(arn string) string {
	if parts := strings.Split(arn, ":"); len(parts) > 4 {
		return parts[4]
	}
	return ""
}

// short spells trusted principals for a note: an account by its ID, a role
// or user by its name, with the account when it is another one.
func short(who []string, own string) []string {
	var out []string
	for _, p := range who {
		switch acct := accountOf(p); {
		case p == "*":
			out = append(out, "anyone")
		case len(p) == 12 && strings.Trim(p, "0123456789") == "":
			out = append(out, "account "+p)
		case strings.HasSuffix(p, ":root"):
			out = append(out, "account "+acct)
		case strings.HasPrefix(p, "arn:"):
			name := p[strings.LastIndex(p, ":")+1:]
			if acct != own {
				name = acct + ":" + name
			}
			out = append(out, name)
		default:
			out = append(out, p)
		}
	}
	if len(out) > 3 {
		out = append(out[:3], fmt.Sprintf("%d more", len(out)-3))
	}
	return out
}

// instances are the account's instances in every region it has enabled,
// swept in parallel: an instance ID carries no region, so nothing less is
// a complete list. A region that refuses (a policy keeping the account out
// of it) is left out; only when every region refuses is that the answer.
// Terminated instances are gone and not listed.
func (r runner) instances(ctx context.Context) ([]proxy.Choice, error) {
	out, err := r.run(ctx, "ec2 describe-regions", "ec2", "describe-regions", "--query", "Regions[].RegionName", "--output", "json")
	if err != nil {
		return nil, err
	}
	var regions []string
	if err := json.Unmarshal(out, &regions); err != nil {
		return nil, fmt.Errorf("unexpected answer from ec2 describe-regions: %w", err)
	}
	sort.Strings(regions)
	type answer struct {
		cs  []proxy.Choice
		err error
	}
	answers := make([]answer, len(regions))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, region := range regions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			answers[i].cs, answers[i].err = r.region(ctx, region)
		}()
	}
	wg.Wait()
	var cs []proxy.Choice
	var first error
	for _, a := range answers {
		cs = append(cs, a.cs...)
		if a.err != nil && first == nil {
			first = a.err
		}
	}
	if first != nil && len(cs) == 0 {
		return nil, first
	}
	sortChoices(cs)
	return cs, nil
}

func (r runner) region(ctx context.Context, region string) ([]proxy.Choice, error) {
	out, err := r.run(ctx, "ec2 describe-instances in "+region, "ec2", "describe-instances", "--region", region,
		"--query", "Reservations[].Instances[].[InstanceId,State.Name,Tags[?Key=='Name']|[0].Value]", "--output", "json")
	if err != nil {
		return nil, err
	}
	var rows [][]*string
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("unexpected answer from ec2 describe-instances in %s: %w", region, err)
	}
	var cs []proxy.Choice
	for _, row := range rows {
		if len(row) < 3 || row[0] == nil {
			continue
		}
		state := deref(row[1])
		if state == "terminated" || state == "shutting-down" {
			continue
		}
		note := []string{}
		if name := deref(row[2]); name != "" {
			note = append(note, name)
		}
		note = append(note, region)
		if state != "running" && state != "" {
			note = append(note, state)
		}
		cs = append(cs, proxy.Choice{Value: *row[0], Note: strings.Join(note, " · ")})
	}
	return cs, nil
}

func (r runner) buckets(ctx context.Context) ([]proxy.Choice, error) {
	out, err := r.run(ctx, "s3api list-buckets", "s3api", "list-buckets", "--query", "Buckets[].[Name,BucketRegion]", "--output", "json")
	if err != nil {
		return nil, err
	}
	var rows [][]*string
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("unexpected answer from s3api list-buckets: %w", err)
	}
	var cs []proxy.Choice
	for _, row := range rows {
		if len(row) == 0 || row[0] == nil {
			continue
		}
		c := proxy.Choice{Value: *row[0]}
		if len(row) > 1 {
			c.Note = deref(row[1])
		}
		cs = append(cs, c)
	}
	sortChoices(cs)
	return cs, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func sortChoices(cs []proxy.Choice) {
	sort.Slice(cs, func(i, j int) bool { return strings.ToLower(cs[i].Value) < strings.ToLower(cs[j].Value) })
}
