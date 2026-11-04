package awsproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/psps/internal/proxy"
)

// Strict SSM: an exposure with ssm=strict gets credentials with no SSM in
// them, so neither a session nor a command reaches an instance from the
// isolation by any route. An agent asks psps instead, a person sees the
// command in the agent's conversation and accepts or declines it, and psps
// runs it with credentials of its own, scoped to that one instance, and
// hands back what it printed.
//
// An interactive session is not offered at all: what goes over one is
// keystrokes, with no command in it to show anyone first.

const (
	SSMDirect = "direct"
	SSMStrict = "strict"
)

// Settings are an aws exposure's per-isolation switches.
type Settings struct {
	SSM string `json:"ssm,omitempty"`
}

func (s Settings) Strict() bool { return s.SSM == SSMStrict }

// Run limits: AWS-RunShellScript's own, and how long psps waits on one.
const (
	MaxRunTimeout     = 3600
	DefaultRunTimeout = 120
)

// RunRequest is what an agent asks to run.
type RunRequest struct {
	Env      string `json:"env"`
	Instance string `json:"instance"`
	Command  string `json:"command"`
	// Timeout is the command's own limit, in seconds.
	Timeout int `json:"timeout,omitempty"`
}

// RunResult is what came back.
type RunResult struct {
	Instance string `json:"instance"`
	Region   string `json:"region"`
	// Status is SSM's: Success, Failed, TimedOut, Cancelled...
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	// User is who it ran as on the instance.
	User      string `json:"user"`
	CommandID string `json:"command_id"`
}

// CanRun refuses an instance the env's allowlist does not name. Either
// capability will do: in strict mode both are served as a command.
func (e Env) CanRun(instance string) error {
	if !InstanceID.MatchString(instance) {
		return proxy.Deny("%q is not an EC2 instance id (i-<hex>)", instance)
	}
	for _, entry := range e.Instances {
		if name, _, _ := strings.Cut(entry, "="); name == instance {
			return nil
		}
	}
	return proxy.Deny("instance %s is not on this environment's allowlist (%s); the operator adds it with `psps env edit aws ENV --allow-instance %s`",
		instance, describe(e.Instances), instance)
}

// strictPolicy is the isolation's session policy in strict mode: the
// allowlist with every SSM action taken out, so what is left is the
// buckets, Batch, the log groups and finding instances.
func strictPolicy(a Allowlist) (Policy, error) {
	p, err := a.Policy()
	if err != nil {
		return Policy{}, err
	}
	var kept []Statement
	for _, s := range p.Statement {
		switch s.Sid {
		case "Sessions", "SessionLifecycle", "Commands":
			continue
		case "SsmReads":
			s.Action = []string{"ssm:DescribeInstanceInformation"}
		}
		kept = append(kept, s)
	}
	p.Statement = kept
	return p, nil
}

// runPolicy is what psps runs one command with: that instance alone, the
// command's reads, and ending the isolation's own sessions.
func runPolicy(instance, sessionName string) Policy {
	return Policy{Version: "2012-10-17", Statement: []Statement{
		{Sid: "Command", Effect: "Allow", Action: []string{"ssm:SendCommand"},
			Resource: []string{"arn:aws:ec2:*:*:instance/" + instance, "arn:aws:ssm:*::document/AWS-RunShellScript"}},
		{Sid: "CommandReads", Effect: "Allow", Action: []string{"ssm:GetCommandInvocation"}, Resource: []string{"arn:aws:ssm:*:*:*"}},
		{Sid: "EndOwnSessions", Effect: "Allow", Action: []string{"ssm:TerminateSession"},
			Resource: []string{"arn:aws:ssm:*:*:session/" + sessionName + "-*"}},
	}}
}

// SessionName is what the broker names an isolation's role sessions, and
// so what its SSM sessions are owned by.
func SessionName(isolation string) string { return "psps-" + isolation }

// runSessionName is psps's own, for the commands it runs for isolation:
// told apart in CloudTrail, and not matched by the isolation's sessions.
func runSessionName(isolation string) string { return "psps-run-" + isolation }

// runWith runs aws with the given credentials in place of whatever the
// place would use: on the bastion they go in on stdin, so they are on no
// command line there.
func (c CLI) runWith(ctx context.Context, cr Credentials, what string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if c.Bastion == "" {
		cmd = exec.CommandContext(ctx, c.AWS, args...)
		for _, kv := range os.Environ() {
			switch k, _, _ := strings.Cut(kv, "="); k {
			case "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN":
				continue
			}
			cmd.Env = append(cmd.Env, kv)
		}
		cmd.Env = append(cmd.Env, "AWS_ACCESS_KEY_ID="+cr.AccessKeyId, "AWS_SECRET_ACCESS_KEY="+cr.SecretAccessKey, "AWS_SESSION_TOKEN="+cr.Token)
	} else {
		tc, err := TailcatPath()
		if err != nil {
			return nil, err
		}
		ensureMaster(ctx, tc, c.Bastion)
		script := `IFS= read -r a; IFS= read -r b; IFS= read -r c; ` +
			`export AWS_ACCESS_KEY_ID="$a" AWS_SECRET_ACCESS_KEY="$b" AWS_SESSION_TOKEN="$c"; ` +
			`unset AWS_PROFILE AWS_DEFAULT_PROFILE; exec aws "$@"`
		remote := append([]string{"sh", "-c", script, "sh"}, args...)
		cmd = exec.CommandContext(ctx, sshBin, append(sshOpts(tc, c.Bastion), "-o", "ControlMaster=no", "psps@bastion", shellJoin(remote))...)
		cmd.Stdin = strings.NewReader(cr.AccessKeyId + "\n" + cr.SecretAccessKey + "\n" + cr.Token + "\n")
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, proxy.Deny("%s timed out", what)
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return nil, proxy.Deny("%s could not run: %v", what, err)
	}
	if err != nil {
		return nil, c.refusal(what, exit.ExitCode(), stderr.String())
	}
	return out, nil
}

// regions caches where each instance is: an instance ID carries no region,
// and SSM is regional.
var regions sync.Map // instance -> region

// Region is where instance is, looked up once with the env's own lookup
// identity (the bastion's, or the Mac profile's).
func (e Env) Region(ctx context.Context, instance string) (string, error) {
	if r, ok := regions.Load(instance); ok {
		return r.(string), nil
	}
	r := runner{CLI: e.runner()}
	out, err := r.run(ctx, "ec2 describe-regions", "ec2", "describe-regions", "--query", "Regions[].RegionName", "--output", "json")
	if err != nil {
		return "", err
	}
	var all []string
	if err := json.Unmarshal(out, &all); err != nil {
		return "", fmt.Errorf("unexpected answer from ec2 describe-regions: %w", err)
	}
	found := make(chan string, len(all))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, region := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := r.CLI.run(ctx, "ec2 describe-instances", 30*time.Second, "ec2", "describe-instances", "--region", region,
				"--instance-ids", instance, "--query", "Reservations[].Instances[].InstanceId", "--output", "text")
			if err == nil && strings.TrimSpace(string(out)) == instance {
				found <- region
			}
		}()
	}
	wg.Wait()
	close(found)
	for region := range found {
		regions.Store(instance, region)
		return region, nil
	}
	return "", proxy.Deny("instance %s is in no region this account sees", instance)
}

// runScript is the command as AWS-RunShellScript runs it: as ssm-user,
// whom a session would have been, where the instance has one (it is made
// at an instance's first session); as root otherwise, which is said. The
// command goes in as base64, so no quoting of the agent's survives into
// the shell that unpacks it.
func runScript(command string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(command))
	return `PSPS_CMD="$(printf %s '` + b64 + `' | base64 -d)"
if id -u ssm-user >/dev/null 2>&1; then echo "psps-user: ssm-user" >&2; exec runuser -u ssm-user -- bash -c "$PSPS_CMD"; fi
echo "psps-user: root (this instance has no ssm-user yet)" >&2
exec bash -c "$PSPS_CMD"`
}

// Run runs req's command on its instance for isolation, as the env's role
// with only that instance open, and waits for it to end.
func Run(ctx context.Context, e Env, isolation string, req RunRequest) (RunResult, error) {
	if err := e.CanRun(req.Instance); err != nil {
		return RunResult{}, err
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}
	timeout = min(timeout, MaxRunTimeout)
	region, err := e.Region(ctx, req.Instance)
	if err != nil {
		return RunResult{}, err
	}
	src := &Source{AWS: e.cli(), RoleARN: e.RoleARN, Profile: e.Profile, Bastion: e.Bastion,
		SessionName: runSessionName(isolation), Policy: runPolicy(req.Instance, SessionName(isolation)).JSON(), Duration: 900}
	cr, err := src.Get(ctx, time.Now())
	if err != nil {
		return RunResult{}, err
	}
	cli := e.runner()
	input, _ := json.Marshal(map[string]any{
		"InstanceIds":    []string{req.Instance},
		"DocumentName":   "AWS-RunShellScript",
		"Comment":        "psps, for isolation " + isolation,
		"TimeoutSeconds": 60,
		"Parameters": map[string][]string{
			"commands":         {runScript(req.Command)},
			"executionTimeout": {fmt.Sprint(timeout)},
		},
	})
	out, err := cli.runWith(ctx, cr, "ssm send-command", 60*time.Second, "ssm", "send-command", "--region", region,
		"--cli-input-json", string(input), "--query", "Command.CommandId", "--output", "text")
	if err != nil {
		return RunResult{}, err
	}
	res := RunResult{Instance: req.Instance, Region: region, CommandID: strings.TrimSpace(string(out))}
	deadline := time.Now().Add(time.Duration(timeout+90) * time.Second)
	for {
		if err := sleep(ctx, 2*time.Second); err != nil {
			return res, err
		}
		out, err := cli.runWith(ctx, cr, "ssm get-command-invocation", 60*time.Second, "ssm", "get-command-invocation", "--region", region,
			"--command-id", res.CommandID, "--instance-id", req.Instance, "--output", "json")
		if err != nil {
			// Asked too soon after sending, SSM has no invocation yet.
			if strings.Contains(err.Error(), "InvocationDoesNotExist") && time.Now().Before(deadline) {
				continue
			}
			return res, err
		}
		var inv struct {
			Status, StandardOutputContent, StandardErrorContent string
			ResponseCode                                        int
		}
		if err := json.Unmarshal(out, &inv); err != nil {
			return res, fmt.Errorf("unexpected answer from ssm get-command-invocation: %w", err)
		}
		switch inv.Status {
		case "Pending", "InProgress", "Delayed", "Cancelling":
			if time.Now().After(deadline) {
				return res, proxy.Deny("command %s on %s did not finish in %ds", res.CommandID, req.Instance, timeout+90)
			}
			continue
		}
		res.Status, res.ExitCode, res.Stdout = inv.Status, inv.ResponseCode, inv.StandardOutputContent
		res.Stderr, res.User = splitUser(inv.StandardErrorContent)
		return res, nil
	}
}

// splitUser takes runScript's own line out of stderr.
func splitUser(stderr string) (rest, user string) {
	var kept []string
	for _, l := range strings.Split(stderr, "\n") {
		if u, ok := strings.CutPrefix(l, "psps-user: "); ok && user == "" {
			user = u
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n"), user
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// EndSessions ends the SSM sessions isolation's credentials opened that
// are still open, in the regions of the env's instances, and says how
// many. Finding them is the lookup identity's (ssm:DescribeSessions);
// ending them, psps's run credentials'.
func EndSessions(ctx context.Context, e Env, isolation string) (int, error) {
	account, _, _ := strings.Cut(strings.TrimPrefix(e.RoleARN, "arn:aws:iam::"), ":")
	role := e.RoleARN[strings.LastIndex(e.RoleARN, "/")+1:]
	owner := fmt.Sprintf("arn:aws:sts::%s:assumed-role/%s/%s", account, role, SessionName(isolation))
	seen := map[string]bool{}
	var cr *Credentials
	ended := 0
	var errs []string
	for _, entry := range e.Instances {
		name, _, _ := strings.Cut(entry, "=")
		region, err := e.Region(ctx, name)
		if err != nil || seen[region] {
			continue
		}
		seen[region] = true
		out, err := e.runner().run(ctx, "ssm describe-sessions", 60*time.Second, "ssm", "describe-sessions", "--region", region,
			"--state", "Active", "--filters", "key=Owner,value="+owner, "--query", "Sessions[].SessionId", "--output", "json")
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		var ids []string
		json.Unmarshal(out, &ids)
		for _, id := range ids {
			if cr == nil {
				src := &Source{AWS: e.cli(), RoleARN: e.RoleARN, Profile: e.Profile, Bastion: e.Bastion,
					SessionName: runSessionName(isolation), Policy: runPolicy(name, SessionName(isolation)).JSON(), Duration: 900}
				c, err := src.Get(ctx, time.Now())
				if err != nil {
					return ended, err
				}
				cr = &c
			}
			if _, err := e.runner().runWith(ctx, *cr, "ssm terminate-session", 60*time.Second, "ssm", "terminate-session",
				"--region", region, "--session-id", id); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			ended++
		}
	}
	if len(errs) > 0 {
		return ended, errors.New(strings.Join(errs, "; "))
	}
	return ended, nil
}
