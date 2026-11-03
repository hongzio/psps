package awsproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/psps/internal/paths"
	"github.com/hongzio/psps/internal/proxy"
)

// An env may name a bastion: an EC2 instance, reached over tailcat
// (WireGuard with no account or control plane), whose instance role is the
// principal that assumes the env's role. AWS rotates an instance role's
// credentials itself, so unlike the Mac's SSO login nothing expires that a
// person has to renew. The bastion runs
//
//	tailcat serve --allow=<the Mac's client key> no-auth-ssh
//
// and every aws command is run there over ssh. `tailcat ssh` passes no
// ssh options, so the system ssh is run with tailcat as its ProxyCommand:
// BatchMode and a known_hosts of psps's own keep a prompt from ever
// hanging the daemon.
//
// Each tailcat client is a WireGuard peer, and two at once with the Mac's
// one key push each other off: two connections take seconds, eight never
// finish, and the instances lookup runs eight. So a bastion has one ssh
// master, one tailcat, and every command is a channel in it.

// CLI is where an aws command runs: the Mac's aws binary with a profile,
// or a bastion's aws with its instance role.
type CLI struct {
	AWS     string // the Mac's aws binary; unused with a bastion
	Profile string // the Mac's profile; unused with a bastion
	Bastion string // a tailcat address
}

var (
	sshBin = "/usr/bin/ssh"
	// tailcatDirs is where tailcat is looked for when the daemon's PATH,
	// launchd's minimal one, does not have it.
	tailcatDirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}
	// knownHosts is psps's own known_hosts for bastions.
	knownHosts = func() string { return filepath.Join(paths.Default().State, "bastion_known_hosts") }
	// controlPath is the masters' sockets; %C is a hash of the destination,
	// which HostKeyAlias makes one per bastion.
	controlPath = func() string { return filepath.Join(paths.Default().State, "bastion-%C") }

	mastersMu sync.Mutex
	masters   = map[string]*sync.Mutex{} // by address: one master started at a time
)

// masterPersist is how long an idle master stays: past the broker's
// hourly refresh would keep a tunnel up for nothing.
const masterPersist = "10m"

// TailcatPath finds the Mac's tailcat.
func TailcatPath() (string, error) {
	if p, err := exec.LookPath("tailcat"); err == nil {
		return p, nil
	}
	for _, d := range tailcatDirs {
		p := filepath.Join(d, "tailcat")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", proxy.Deny("tailcat is not on the Mac; install it with `brew install tailcat`")
}

// Command is the aws command with args, ready to run.
func (c CLI) Command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if c.Bastion == "" {
		if c.Profile != "" {
			args = append(args[:len(args):len(args)], "--profile", c.Profile)
		}
		return exec.CommandContext(ctx, c.AWS, args...), nil
	}
	tc, err := TailcatPath()
	if err != nil {
		return nil, err
	}
	ensureMaster(ctx, tc, c.Bastion)
	argv := append(sshOpts(tc, c.Bastion), "-o", "ControlMaster=no", "psps@bastion", shellJoin(append([]string{"aws"}, args...)))
	return exec.CommandContext(ctx, sshBin, argv...), nil
}

// ensureMaster starts the bastion's ssh master unless one runs. Whether
// it starts is not reported: a command with no master to use connects by
// itself, and says why if it cannot.
func ensureMaster(ctx context.Context, tc, addr string) {
	mastersMu.Lock()
	mu, ok := masters[addr]
	if !ok {
		mu = &sync.Mutex{}
		masters[addr] = mu
	}
	mastersMu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	opts := sshOpts(tc, addr)
	if exec.CommandContext(ctx, sshBin, append(opts, "-O", "check", "psps@bastion")...).Run() == nil {
		return
	}
	// -f: ssh returns once connected and the master stays behind it. No
	// output is read, as the master would hold the pipe open for good.
	exec.CommandContext(ctx, sshBin, append(opts, "-f", "-N",
		"-o", "ControlMaster=yes", "-o", "ControlPersist="+masterPersist,
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2",
		"psps@bastion")...).Run()
}

func sshOpts(tailcat, addr string) []string {
	// A failure here is ssh's to tell, about the file itself.
	_ = os.MkdirAll(filepath.Dir(knownHosts()), 0o700)
	sum := sha256.Sum256([]byte(addr))
	return []string{"-T",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=20",
		"-o", "LogLevel=ERROR",
		"-o", "ProxyCommand=" + shellQuote(tailcat) + " " + shellQuote(addr) + " 22",
		// Named as when psps was gero, so a bastion's host key recorded
		// then still holds.
		"-o", "HostKeyAlias=gero-bastion-" + hex.EncodeToString(sum[:6]),
		"-o", "UserKnownHostsFile=" + knownHosts(),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ControlPath=" + controlPath()}
}

// shellJoin is argv as one sh command line: ssh hands the remote side a
// single string, and the session policy is JSON full of quotes.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// where names the place a command ran, for its errors.
func (c CLI) where() string {
	if c.Bastion != "" {
		return "the bastion"
	}
	return "the Mac"
}

// run runs one aws command, its stdout or why not.
func (c CLI) run(ctx context.Context, what string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := c.Command(ctx, args...)
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		if c.Bastion != "" {
			return nil, unreachable(c.Bastion, what+" timed out")
		}
		return nil, proxy.Deny("%s timed out; is the network up?", what)
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		if c.Bastion != "" {
			return nil, proxy.Deny("ssh %q is not runnable: %v", sshBin, err)
		}
		return nil, proxy.Deny("aws CLI %q is not runnable: %v", c.AWS, err)
	}
	if err != nil {
		return nil, c.refusal(what, exit.ExitCode(), stderr.String())
	}
	return out, nil
}

// refusal is what a failed aws command tells the operator. Through a
// bastion, ssh's own failure (exit 255) is the bastion not reached, and
// aws not found there is named; the rest is AWS's, passed on as spelled.
func (c CLI) refusal(what string, code int, stderr string) error {
	if c.Bastion == "" {
		return refusal(what, c.Profile, stderr)
	}
	msg := strings.TrimSpace(stderr)
	if len(msg) > 300 {
		msg = msg[:300]
	}
	switch {
	case code == 255:
		return unreachable(c.Bastion, msg)
	case code == 127:
		return proxy.Deny("the bastion has no aws CLI on its PATH; install it there (Amazon Linux has it)")
	}
	return proxy.Deny("%s on the bastion failed: %s", what, msg)
}

func unreachable(addr, why string) error {
	short := addr
	if len(short) > 24 {
		short = short[:24] + "…"
	}
	if why == "" {
		why = "no answer"
	}
	return proxy.Deny("the bastion %s did not answer (%s); is the instance up, `tailcat serve ... no-auth-ssh` running on it, and the Mac's tailcat client key (`tailcat printpub` prints it) in its --allow?", short, why)
}

// Identity is who the aws CLI is, there: get-caller-identity's ARN.
func (c CLI) Identity(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "sts get-caller-identity", 60*time.Second, "sts", "get-caller-identity", "--output", "json")
	if err != nil {
		return "", err
	}
	var id struct{ Arn string }
	if json.Unmarshal(out, &id) != nil || id.Arn == "" {
		return "", proxy.Deny("sts get-caller-identity on %s answered no identity", c.where())
	}
	return id.Arn, nil
}

// checkBastion is what saving an env with a bastion asks first: that the
// bastion answers, as an AWS principal. A test replaces it.
var checkBastion = func(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := CLI{Bastion: addr}.Identity(ctx)
	return err
}

// validBastion refuses what is not one tailcat address (or DNS name).
func validBastion(addr string) error {
	if addr == "" {
		return nil
	}
	for _, r := range addr {
		if r <= ' ' || r == 0x7f || r == '\'' || r == '"' || r == '\\' {
			return fmt.Errorf("%q is not a tailcat address; give the one `tailcat serve` printed on the bastion", addr)
		}
	}
	return nil
}
