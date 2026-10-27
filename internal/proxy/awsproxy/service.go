package awsproxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

const (
	Port      = 3021
	CredsPath = "/creds"
)

// Env is one AWS environment: a role, the host profile allowed to assume
// it, and the allowlist the session policy is built from. dev and prod are
// separate accounts, so an isolation may have several at once.
type Env struct {
	RoleARN   string   `json:"role_arn"`
	Profile   string   `json:"aws_profile"`
	Instances []string `json:"instances"`
	Buckets   []string `json:"buckets"`
	CLI       string   `json:"aws_cli,omitempty"`
}

func (e Env) cli() string {
	if e.CLI != "" {
		return e.CLI
	}
	return "aws"
}

type Service struct{}

func (Service) Name() string { return "aws" }
func (Service) Ports() []int { return []int{Port} }
func (Service) Summary() string {
	return "scoped, short-lived AWS credentials from an assumed role, for SSM and S3"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "role_arn", Kind: proxy.String, Flag: "role-arn", Metavar: "ARN",
			Help: "the IAM role the broker assumes (required)"},
		{Key: "aws_profile", Kind: proxy.String, Flag: "aws-profile", Metavar: "NAME",
			Help: "the Mac's AWS CLI profile whose login may assume the role (required)"},
		{Key: "instances", Kind: proxy.List, Flag: "allow-instance", Clear: "clear-instances", Metavar: "I-ID[=CAPS]",
			Help: "an EC2 instance SSM may reach; =session or =command narrows it (repeat; replaces the list)"},
		{Key: "buckets", Kind: proxy.List, Flag: "allow-bucket", Clear: "clear-buckets", Metavar: "BUCKET[=CAPS]",
			Help: "an S3 bucket objects may move through; =read, =write, =delete narrow it (repeat; replaces the list)"},
		{Key: "aws_cli", Kind: proxy.String, Flag: "aws-cli", Metavar: "BIN",
			Help: "the Mac's aws binary (default aws on PATH, pinned to its full path)"},
	}
}

func (Service) Settings() []proxy.Field                { return nil }
func (Service) NormalizeSettings(map[string]any) error { return nil }

func (Service) Normalize(name string, env map[string]any) ([]string, error) {
	// Canonical, capability-merged spellings, so a bad capability fails
	// here with its vocabulary rather than at the broker's first request.
	instances, err := MergeEntries(proxy.Strings(env, "instances"), Instance)
	if err != nil {
		return nil, err
	}
	buckets, err := MergeEntries(proxy.Strings(env, "buckets"), Bucket)
	if err != nil {
		return nil, err
	}
	env["instances"], env["buckets"] = instances, buckets
	role := proxy.StringOr(env, "role_arn", "")
	if role == "" {
		return nil, fmt.Errorf("--role-arn is required; it names the IAM role the broker assumes for %q", name)
	}
	if !strings.HasPrefix(role, "arn:aws:iam::") {
		return nil, fmt.Errorf("%q does not look like an IAM role ARN (arn:aws:iam::<account>:role/<name>)", role)
	}
	if proxy.StringOr(env, "aws_profile", "") == "" {
		return nil, fmt.Errorf("--aws-profile is required; it names the Mac's AWS CLI profile whose login may assume the role for %q", name)
	}
	// Pinned now: the daemon may run under launchd's minimal PATH.
	cli := proxy.StringOr(env, "aws_cli", "aws")
	found, err := exec.LookPath(cli)
	if err != nil {
		return nil, fmt.Errorf("%q is not on PATH; install the AWS CLI on the Mac or pass --aws-cli with a full path", cli)
	}
	env["aws_cli"] = found
	if len(instances) == 0 && len(buckets) == 0 {
		return []string{fmt.Sprintf("env %q has an empty allowlist; the broker refuses to issue its credentials until --allow-instance/--allow-bucket", name)}, nil
	}
	// The allowlist is checked against STS's size limit here, where the
	// operator hears it, not at the first request, where the agent does.
	if _, err := SessionPolicy(instances, buckets); err != nil {
		return nil, err
	}
	return nil, nil
}

// TokenKeys: one token per isolation. The env is chosen by URL path, as
// isolate does, so another env for one command is just another URI.
func (Service) TokenKeys(store.Exposure) []string { return []string{""} }

func decodeEnvs(v proxy.View) (map[string]Env, error) {
	envs := map[string]Env{}
	for _, name := range v.Exposure.Envs {
		if _, ok := v.Envs[name]; !ok {
			continue
		}
		var e Env
		if err := v.Decode(name, &e); err != nil {
			return nil, fmt.Errorf("aws env %s: %w", name, err)
		}
		envs[name] = e
	}
	return envs, nil
}

func (Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	envs, err := decodeEnvs(v)
	if err != nil {
		return proxy.Workspace{}, err
	}
	return proxy.Workspace{
		// Environment rather than ~/.aws on purpose: every SDK in play reads
		// it natively, and turning the service off removes it instead of
		// leaving a config that points at nothing.
		Env: []string{
			fmt.Sprintf("AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:%d%s", Port, CredsPath),
			"AWS_CONTAINER_AUTHORIZATION_TOKEN=" + v.Tokens[""],
		},
		Files: proxy.SkillFiles("aws", skillBody(v, envs)),
	}, nil
}

func (Service) Managed() []string { return proxy.SkillDirs("aws") }

func (Service) New(h proxy.Host) proxy.Instance {
	in := &instance{h: h, sources: map[string]*cached{}}
	in.srv = &http.Server{Handler: in, ReadHeaderTimeout: 30 * time.Second}
	return in
}

type instance struct {
	h   proxy.Host
	srv *http.Server

	mu      sync.Mutex
	sources map[string]*cached // by env name
}

// cached is an env's Source and what it was built from, so an edit to the
// env (a narrower allowlist, another role) is a new Source at once rather
// than the old credentials until they expire.
type cached struct {
	key string
	src *Source
}

func (in *instance) Serve(port int, l net.Listener) error {
	if port != Port {
		return fmt.Errorf("aws broker: no service on port %d", port)
	}
	return in.srv.Serve(l)
}

func (in *instance) logf(format string, a ...any) { in.h.Logf("aws", format, a...) }

func (in *instance) source(env string, e Env, policy string) *Source {
	key := strings.Join([]string{e.cli(), e.RoleARN, e.Profile, policy}, "\x00")
	in.mu.Lock()
	defer in.mu.Unlock()
	if c, ok := in.sources[env]; ok && c.key == key {
		return c.src
	}
	src := &Source{AWS: e.cli(), RoleARN: e.RoleARN, Profile: e.Profile,
		SessionName: "gero-" + in.h.Isolation, Policy: policy}
	in.sources[env] = &cached{key: key, src: src}
	return src
}

func send(w http.ResponseWriter, code int, payload any) {
	b, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)+1))
	w.WriteHeader(code)
	w.Write(append(b, '\n'))
}

func reason(r string) map[string]string { return map[string]string{"reason": r} }

func (in *instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	if path != CredsPath && !strings.HasPrefix(path, CredsPath+"/") {
		send(w, 404, reason("only "+CredsPath+" exists here"))
		return
	}
	if r.Method != http.MethodGet {
		send(w, 405, reason(CredsPath+" answers GET only"))
		return
	}
	v, err := in.h.View()
	if err != nil {
		send(w, 503, reason(err.Error()))
		return
	}
	if !v.Enabled {
		msg := fmt.Sprintf("the AWS broker is off for isolation %s; the operator turns it on with `gero iso expose %s aws <env>`", in.h.Isolation, in.h.Isolation)
		send(w, 503, reason(msg))
		in.logf("DENY %s: off", r.URL.Path)
		return
	}
	want := v.Tokens[""]
	offered := r.Header.Get("Authorization")
	if want == "" || subtle.ConstantTimeCompare([]byte(offered), []byte(want)) != 1 {
		send(w, 401, reason("missing or wrong authorization token; this endpoint serves the isolation's AWS_CONTAINER_AUTHORIZATION_TOKEN only"))
		in.logf("DENY %s: bad token", r.URL.Path)
		return
	}
	name := strings.TrimPrefix(strings.TrimPrefix(path, CredsPath), "/")
	if name == "" {
		name = v.Exposure.Default
	}
	envs, err := decodeEnvs(v)
	if err != nil {
		send(w, 503, reason(err.Error()))
		return
	}
	e, ok := envs[name]
	if !ok {
		send(w, 404, reason(unknownEnv(name, v)))
		in.logf("DENY %s: unknown environment", r.URL.Path)
		return
	}
	// An empty allowlist still answers, because "connection refused" teaches
	// nothing while a 503 names the fix.
	if len(e.Instances) == 0 && len(e.Buckets) == 0 {
		msg := fmt.Sprintf("environment %q has an empty allowlist; the operator adds to it with `gero env edit aws %s --allow-instance i-...` or `--allow-bucket name`", name, name)
		send(w, 503, reason(msg))
		in.logf("FAIL %s: %s", r.URL.Path, msg)
		return
	}
	policy, err := SessionPolicy(e.Instances, e.Buckets)
	if err == nil {
		var c Credentials
		if c, err = in.source(name, e, policy.JSON()).Get(r.Context(), time.Now()); err == nil {
			send(w, 200, c)
			in.logf("ISSUE %s expires %s", name, c.Expiration)
			return
		}
	}
	send(w, 503, reason(err.Error()))
	in.logf("FAIL %s: %s", r.URL.Path, err)
}

func unknownEnv(name string, v proxy.View) string {
	for _, m := range v.Missing {
		if m == name {
			return fmt.Sprintf("environment %q was removed from gero's registry but is still exposed here; the operator re-exposes with `gero iso expose %s aws ...`", name, v.Isolation)
		}
	}
	have := make([]string, 0, len(v.Envs))
	for n := range v.Envs {
		have = append(have, n)
	}
	sort.Strings(have)
	list := strings.Join(have, ", ")
	if list == "" {
		list = "(none)"
	}
	if name == "" {
		return fmt.Sprintf("no default AWS environment here; this broker serves: %s. Point AWS_CONTAINER_CREDENTIALS_FULL_URI at %s/<env> to pick one", list, CredsPath)
	}
	return fmt.Sprintf("no AWS environment named %q here; this broker serves: %s. Point AWS_CONTAINER_CREDENTIALS_FULL_URI at %s/<env> to pick one", name, list, CredsPath)
}

// Check assumes the role from the Mac, now, and asks STS who that is.
// Refusals: the credential endpoint answers the isolation's token only.
func (Service) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong authorization token", Port: Port, Method: "GET", Path: CredsPath,
		Header: map[string]string{"Authorization": proxy.WrongToken}, Status: 401, Want: "missing or wrong authorization token"}}
}

func (Service) Check(ctx context.Context, name string, cfg json.RawMessage, _ *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	policy, err := SessionPolicy(e.Instances, e.Buckets)
	if err != nil {
		return "", err
	}
	src := &Source{AWS: e.cli(), RoleARN: e.RoleARN, Profile: e.Profile,
		SessionName: "gero-envcheck-" + name, Policy: policy.JSON()}
	c, err := src.Get(ctx, time.Now())
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.cli(), "sts", "get-caller-identity", "--output", "json")
	for _, kv := range os.Environ() {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN":
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, "AWS_ACCESS_KEY_ID="+c.AccessKeyId, "AWS_SECRET_ACCESS_KEY="+c.SecretAccessKey, "AWS_SESSION_TOKEN="+c.Token)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("credentials were issued but get-caller-identity failed: %.300s", strings.TrimSpace(stderr.String()))
	}
	var id struct{ Arn string }
	json.Unmarshal(out, &id)
	return fmt.Sprintf("identity   %s\nexpires    %s\ninstances  %s\nbuckets    %s\n",
		id.Arn, c.Expiration, describe(e.Instances), describe(e.Buckets)), nil
}
