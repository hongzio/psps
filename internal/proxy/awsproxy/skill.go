package awsproxy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

// Config cannot express why an AccessDenied happened, or that it is final;
// the skill does.

const skillTemplate = `---
name: gero-aws
description: How AWS access works inside this gero isolation. Use when an AWS
  call fails with AccessDeniedException or NoCredentialsError, when an SSM
  session or an ansible aws_ssm task is refused, or when deciding whether an
  AWS resource is reachable from here.
---

# AWS in this isolation

There is no AWS credential file here, and there is not meant to be one. The
standard SDK credential chain reads AWS_CONTAINER_CREDENTIALS_FULL_URI from the
environment and fetches short-lived credentials from a broker outside the
isolation. The broker assumes an IAM role and attaches a session policy built
from the operator's allowlist, so AWS itself enforces what this isolation may
touch. ` + "`aws`" + `, boto3 and ansible's aws_ssm connection all pick this up with no
~/.aws. session-manager-plugin is installed.

The AWS CLI itself is not preinstalled. If you need it, install it into your
home (no root needed):

    cd /tmp && curl -fsSLo awscliv2.zip "https://awscli.amazonaws.com/awscli-exe-linux-$(uname -m).zip" \
      && unzip -q awscliv2.zip && ./aws/install -i ~/.local/aws-cli -b ~/.local/bin

and run it as ` + "`~/.local/bin/aws`" + ` (or put ~/.local/bin on PATH).

Do not set AWS_PROFILE, do not write ~/.aws/credentials, and do not unset the
container variables: any of those disconnects the working chain and none of
them can add a credential.

` + "`aws sts get-caller-identity`" + ` always answers and names the scoped role.

## Environments

The broker serves one credential set per environment:

%[1]s

The default environment (what the exported variable already points at) is
` + "`%[2]s`" + `. To run one command against another environment, override the
URI for just that command:

    AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:%[3]d/creds/<env> <command>

An entry's ` + "`=`" + ` suffix narrows what it permits — instances: ` + "`session`" + ` (SSM
sessions), ` + "`command`" + ` (SendCommand); buckets: ` + "`read`, `write`, `delete`" + `. A bare
name permits its full set. The narrowing is inside the issued credentials
themselves, so a refused action fails at AWS with the session-policy message
below, not at the broker.

## When a call is refused

- ` + "`AccessDeniedException ... because no session policy allows <action>`" + ` — the
  resource or capability is outside this environment's allowlist. That is the
  operator's decision, not a fault to retry or route around. If access is
  genuinely needed, ask the operator to run
  ` + "`gero env edit aws <env> --allow-instance i-...`" + ` (or ` + "`--allow-bucket name`" + `).
- ` + "`... because no identity-based policy allows <action>`" + ` — the allowlist
  already permits it, but the IAM role the broker assumes does not. AWS grants
  only what both the role's own policy and the session policy allow, so no
  ` + "`gero env edit`" + ` can fix this one. Tell the operator the role ARN from the
  message and the action and resource it was refused; widening the role's
  policy in that account is theirs to do. Do not retry.
- The credential endpoint answers 503 — read the reason directly:
  ` + "`curl -sS -H \"Authorization: $AWS_CONTAINER_AUTHORIZATION_TOKEN\" \"$AWS_CONTAINER_CREDENTIALS_FULL_URI\"`" + `.
  The usual cause is the Mac's own AWS login having expired; only the operator
  can fix that, so report it rather than retrying in a loop.
- ` + "`Unable to locate credentials`" + ` / NoCredentialsError — this shell predates the
  broker. Start a fresh shell so the exported variables are present.

## Do not try to route around it

Switching regions, endpoints or ARN spellings does not widen the allowlist: AWS
checks the session policy server-side on every call. There is no other
credential in this isolation to fall back to — only the scoped ones the broker
issues, and the broker is the only thing the token in this environment opens.
`

func skillBody(v proxy.View, envs map[string]Env) string {
	names := make([]string, 0, len(envs))
	for n := range envs {
		names = append(names, n)
	}
	sort.Strings(names)
	var lines []string
	for _, n := range names {
		e := envs[n]
		lines = append(lines, fmt.Sprintf("- `%s` — instances: %s; buckets: %s", n, describe(e.Instances), describe(e.Buckets)))
	}
	list := strings.Join(lines, "\n")
	if list == "" {
		list = "(none exposed yet)"
	}
	def := v.Exposure.Default
	if def == "" {
		def = "(none)"
	}
	return fmt.Sprintf(skillTemplate, list, def, Port)
}
