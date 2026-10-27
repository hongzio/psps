// Package awsproxy is the AWS credential broker, a port of isolate's
// awscreds.py: the isolation gets scoped, short-lived credentials, never the
// Mac's.
//
//	isolation  AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:3021/creds
//	             |  (the isolation's tunnel)
//	daemon     this broker: checks the workspace token, then
//	             `aws sts assume-role` with a session policy built from the
//	             env's allowlist (--allow-instance / --allow-bucket)
//	             |
//	           scoped credentials, nothing written to disk
//
// Every SDK that matters (botocore under ansible, the AWS CLI, the Go SDKs)
// reads that variable as a first-class credential source, so the isolation
// needs no ~/.aws, and session-manager-plugin never needs credentials at
// all, which is what made this shape beat a SigV4 re-signing proxy: SSM's
// WebSocket data channel stays untouched.
//
// The allowlist is enforced by AWS, not here: the session policy names
// exactly the allowed instances and buckets, and a call outside it is
// refused upstream with the action, the resource and "because no session
// policy allows..." spelled out. The broker's own refusals carry their
// reason as JSON.
//
// There is no read-only mode, deliberately: git separates fetch from push
// at the protocol level, but an SSM session is a shell and IAM cannot see
// what runs inside it. The boundary is which instances open at all, and the
// account the role lives in.
package awsproxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

// What the policy may name: instance IDs as EC2 issues them, bucket names
// per S3's rules (the subset that matters). Anything else is refused before
// it reaches a policy document, because a wildcard smuggled into a Resource
// ARN would widen what AWS enforces.
var (
	InstanceID = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
	BucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

type Kind string

const (
	Instance Kind = "instance"
	Bucket   Kind = "bucket"
)

// Caps is what an allowlist entry may narrow itself to: `i-...=session`
// opens SSM sessions but cannot SendCommand; `bucket=read` fetches objects
// but cannot write or delete them. A bare name keeps the full set.
var Caps = map[Kind][]string{
	Instance: {"session", "command"},
	Bucket:   {"read", "write", "delete"},
}

// parseEntry returns an allowlist entry's name and capabilities. The
// vocabulary is named for what the operator means, not for IAM actions:
// the mapping to actions is this package's job.
func parseEntry(entry string, kind Kind) (string, map[string]bool, error) {
	name, suffix, sep := strings.Cut(entry, "=")
	re := InstanceID
	if kind == Bucket {
		re = BucketName
	}
	if !re.MatchString(name) {
		if kind == Instance {
			return "", nil, proxy.Deny("%q is not an EC2 instance id (i-<hex>)", name)
		}
		return "", nil, proxy.Deny("%q is not a valid S3 bucket name", name)
	}
	caps := map[string]bool{}
	if !sep {
		for _, c := range Caps[kind] {
			caps[c] = true
		}
		return name, caps, nil
	}
	for _, c := range strings.Split(suffix, ",") {
		if c = strings.TrimSpace(c); c != "" {
			caps[c] = true
		}
	}
	if len(caps) == 0 {
		return "", nil, proxy.Deny("%q names no capability; drop the '=' for the full set", entry)
	}
	var unknown []string
	for c := range caps {
		if !contains(Caps[kind], c) {
			unknown = append(unknown, c)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		word := "capabilities"
		if len(unknown) == 1 {
			word = "capability"
		}
		return "", nil, proxy.Deny("%q: unknown %s %s (valid: %s)", entry, word,
			strings.Join(unknown, ", "), strings.Join(Caps[kind], ", "))
	}
	return name, caps, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// spell is the one spelling of a name and its capabilities: sorted, and the
// full set collapsed to the bare name.
func spell(name string, caps map[string]bool, kind Kind) string {
	if len(caps) == len(Caps[kind]) {
		return name
	}
	l := make([]string, 0, len(caps))
	for c := range caps {
		l = append(l, c)
	}
	sort.Strings(l)
	return name + "=" + strings.Join(l, ",")
}

// CanonicalEntry is one spelling per meaning, so stored config compares
// cleanly.
func CanonicalEntry(entry string, kind Kind) (string, error) {
	name, caps, err := parseEntry(entry, kind)
	if err != nil {
		return "", err
	}
	return spell(name, caps, kind), nil
}

// MergeEntries is the canonical allowlist: entries for the same name union
// their capabilities, so `--allow-instance i-x=session --allow-instance
// i-x=command` is stored as the one entry it means.
func MergeEntries(entries []string, kind Kind) ([]string, error) {
	merged := map[string]map[string]bool{}
	for _, e := range entries {
		name, caps, err := parseEntry(e, kind)
		if err != nil {
			return nil, err
		}
		if merged[name] == nil {
			merged[name] = map[string]bool{}
		}
		for c := range caps {
			merged[name][c] = true
		}
	}
	out := []string{}
	for name, caps := range merged {
		out = append(out, spell(name, caps, kind))
	}
	sort.Strings(out)
	return out, nil
}

// PolicyLimit is STS's cap on a session policy's plaintext size. Refused
// here first, with the limit named, so the operator hears "the allowlist is
// too large" instead of an STS validation error mid-request.
const PolicyLimit = 2048

type Statement struct {
	Sid      string   `json:"Sid"`
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

type Policy struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}

// JSON is the policy as STS receives it, compact.
func (p Policy) JSON() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// SessionPolicy is the allowlist as IAM will enforce it.
//
// Statements split two ways. By how actions authorize, which isolate
// measured rather than assumed: StartSession and SendCommand check the
// instance ARN and the document ARN, so they pin to named instances, while
// DescribeInstanceInformation and the command reads authorize against
// arn:aws:ssm:region:account:* with no instance in sight, so they get their
// own broad statement (the ec2 ARN grammar keeps that breadth away from the
// session actions). And by capability, so `i-...=session` or `bucket=read`
// lands its resource in exactly the statements its suffix names.
func SessionPolicy(instances, buckets []string) (Policy, error) {
	type parsed struct {
		name string
		caps map[string]bool
	}
	parse := func(entries []string, kind Kind) ([]parsed, error) {
		var out []parsed
		for _, e := range entries {
			name, caps, err := parseEntry(e, kind)
			if err != nil {
				return nil, err
			}
			out = append(out, parsed{name, caps})
		}
		return out, nil
	}
	ins, err := parse(instances, Instance)
	if err != nil {
		return Policy{}, err
	}
	bks, err := parse(buckets, Bucket)
	if err != nil {
		return Policy{}, err
	}
	if len(ins) == 0 && len(bks) == 0 {
		return Policy{}, proxy.Deny("the AWS allowlist is empty; add --allow-instance i-... or --allow-bucket name before credentials can be issued")
	}

	documents := []string{"arn:aws:ssm:*:*:document/*", "arn:aws:ssm:*::document/AWS-RunShellScript"}
	var sessionARNs, commandARNs []string
	for _, i := range ins {
		arn := "arn:aws:ec2:*:*:instance/" + i.name
		if i.caps["session"] {
			sessionARNs = append(sessionARNs, arn)
		}
		if i.caps["command"] {
			commandARNs = append(commandARNs, arn)
		}
	}
	var st []Statement
	allow := func(sid string, actions, resources []string) {
		st = append(st, Statement{Sid: sid, Effect: "Allow", Action: actions, Resource: resources})
	}
	if len(sessionARNs) > 0 {
		allow("Sessions", []string{"ssm:StartSession"}, append(sessionARNs, documents...))
		allow("SessionLifecycle", []string{"ssm:TerminateSession"}, []string{"arn:aws:ssm:*:*:session/*"})
	}
	if len(commandARNs) > 0 {
		allow("Commands", []string{"ssm:SendCommand"}, append(commandARNs, documents...))
	}
	if len(ins) > 0 {
		allow("SsmReads", []string{"ssm:DescribeInstanceInformation", "ssm:GetCommandInvocation", "ssm:ListCommandInvocations"},
			[]string{"arn:aws:ssm:*:*:*"})
	}
	for _, s := range []struct {
		sid, cap string
		actions  []string
	}{
		{"TransferRead", "read", []string{"s3:GetObject", "s3:ListBucket"}},
		{"TransferWrite", "write", []string{"s3:PutObject"}},
		{"TransferDelete", "delete", []string{"s3:DeleteObject"}},
	} {
		var res []string
		for _, b := range bks {
			if b.caps[s.cap] {
				res = append(res, "arn:aws:s3:::"+b.name, "arn:aws:s3:::"+b.name+"/*")
			}
		}
		if len(res) > 0 {
			allow(s.sid, s.actions, res)
		}
	}
	p := Policy{Version: "2012-10-17", Statement: st}
	if size := len(p.JSON()); size > PolicyLimit {
		return Policy{}, proxy.Deny("the session policy is %d characters; STS caps it at %d. Narrow the allowlist (%d instances, %d buckets)",
			size, PolicyLimit, len(ins), len(bks))
	}
	return p, nil
}

// describe is an allowlist in a line, for the skill.
func describe(entries []string) string {
	if len(entries) == 0 {
		return "(none)"
	}
	if len(entries) <= 8 {
		return strings.Join(entries, ", ")
	}
	return fmt.Sprintf("%d entries; try the call and read the refusal if there is one", len(entries))
}
