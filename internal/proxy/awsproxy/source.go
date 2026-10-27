package awsproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/proxy"
)

// RefreshMargin is how long before expiry the broker assumes the role again.
// botocore treats credentials with under ten minutes left as expired and
// refetches on every call; staying ahead of that window means the isolation
// never gets credentials it immediately distrusts.
const RefreshMargin = 900 * time.Second

// Credentials is the container-provider wire shape: Token, not SessionToken.
type Credentials struct {
	AccessKeyId     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

// Source assumes the role on demand, briefly cached, nothing on disk.
//
// The host credential behind Profile is whatever the AWS CLI resolves, an
// SSO or `aws login` session today. It expires, and that is the one failure
// an agent cannot fix from inside the isolation, so it maps to a reason
// naming the operator's command rather than whatever stderr said.
type Source struct {
	AWS         string // the aws binary, pinned to a full path
	RoleARN     string
	Profile     string
	SessionName string
	Policy      string // compact JSON
	Duration    int    // seconds; 3600 when zero

	mu      sync.Mutex
	creds   *Credentials
	expires time.Time
}

func (s *Source) Get(ctx context.Context, now time.Time) (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creds != nil && now.Before(s.expires.Add(-RefreshMargin)) {
		return *s.creds, nil
	}
	c, err := s.assume(ctx)
	if err != nil {
		// A failure is not cached: the next request tries again.
		return Credentials{}, err
	}
	// An unparseable expiry means no caching, not no credentials.
	exp, _ := time.Parse(time.RFC3339, c.Expiration)
	s.creds, s.expires = &c, exp
	return c, nil
}

func (s *Source) assume(ctx context.Context) (Credentials, error) {
	duration := s.Duration
	if duration == 0 {
		duration = 3600
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.AWS, "sts", "assume-role",
		"--profile", s.Profile,
		"--role-arn", s.RoleARN,
		"--role-session-name", s.SessionName,
		"--policy", s.Policy,
		"--duration-seconds", strconv.Itoa(duration),
		"--output", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return Credentials{}, proxy.Deny("sts assume-role timed out; is the network up?")
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return Credentials{}, proxy.Deny("aws CLI %q is not runnable: %v", s.AWS, err)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if low := strings.ToLower(msg); strings.Contains(low, "expired") || strings.Contains(low, "sso") {
			return Credentials{}, proxy.Deny("the host's AWS login session is expired or missing; the operator must run `aws login --profile %s` on the host", s.Profile)
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return Credentials{}, proxy.Deny("sts assume-role failed: %s", msg)
	}
	var out struct {
		Credentials *struct {
			AccessKeyId, SecretAccessKey, SessionToken, Expiration string
		}
	}
	if json.Unmarshal(stdout.Bytes(), &out) != nil || out.Credentials == nil || out.Credentials.AccessKeyId == "" {
		return Credentials{}, proxy.Deny("sts assume-role returned no credentials")
	}
	c := out.Credentials
	return Credentials{AccessKeyId: c.AccessKeyId, SecretAccessKey: c.SecretAccessKey, Token: c.SessionToken, Expiration: c.Expiration}, nil
}
