package isolation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// A transfer moves one file from one isolation to another, for an ask the
// operator approved (internal/daemon's transfers). It is read and written
// as the agent user on both sides, and passes through this process alone,
// which counts and hashes it on the way: what lands is the bytes the
// operator was shown, or nothing.
//
// The file read is a regular file under AgentHome, named by its own path:
// one that is a link, or reached through one, is refused, so that a path
// the operator approved cannot stand for a file elsewhere. It lands in
// InboxDir, under a name of its own, never over a file there.
const InboxDir = AgentHome + "/inbox"

// ErrTransferChanged is a file whose bytes are not those the transfer was
// approved for.
var ErrTransferChanged = errors.New("the file is not the one approved")

// ErrTransferExists is a name already taken in the inbox.
var ErrTransferExists = errors.New("a file of that name is in the inbox already")

// transferCheck refuses $p unless it is a regular file under the agent's
// home, named by its real path. 3 is "not a regular file", 4 "reached
// through a link or outside the home".
const transferCheck = `p=$1
if [ -L "$p" ] || [ ! -f "$p" ]; then echo "not a regular file: $p" >&2; exit 3; fi
r=$(realpath -e -- "$p") || exit 3
case "$r" in ` + AgentHome + `/*) ;; *) echo "outside ` + AgentHome + `: $r" >&2; exit 4;; esac
if [ "$r" != "$p" ]; then echo "reached through a link: $p is $r" >&2; exit 4; fi
`

// TransferPath is whether p may name a file to transfer: absolute, clean,
// under AgentHome.
func TransferPath(p string) error {
	switch {
	case !strings.HasPrefix(p, AgentHome+"/"):
		return fmt.Errorf("%q is not a file under %s", p, AgentHome)
	case strings.ContainsAny(p, "\x00\n\r"):
		return fmt.Errorf("%q is not a path", p)
	}
	for _, part := range strings.Split(strings.TrimPrefix(p, AgentHome+"/"), "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%q is not a clean path: name the file as it is, with no . or .. or //", p)
		}
	}
	return nil
}

// TransferName is whether n may name a file in the inbox: one plain name,
// not a dot file.
func TransferName(n string) error {
	switch {
	case n == "" || strings.HasPrefix(n, "."):
		return fmt.Errorf("%q is not a file name for the inbox: no dot files", n)
	case strings.ContainsAny(n, "/\x00\n\r\\"):
		return fmt.Errorf("%q is not one file name", n)
	case len(n) > 200:
		return fmt.Errorf("a file name is at most 200 bytes")
	}
	return nil
}

// TransferStat is the size and sha256 of the file at path in container,
// as the agent user reads it, refusing one TransferPath or transferCheck
// refuses.
func (c CLI) TransferStat(ctx context.Context, container, path string) (int64, string, error) {
	if err := TransferPath(path); err != nil {
		return 0, "", err
	}
	var out, errb bytes.Buffer
	cmd := c.Exec(ctx, container, ExecOpts{User: AgentUser, Env: []string{"HOME=" + AgentHome}}, "sh", "-c",
		transferCheck+`stat -c %s -- "$p" && sha256sum -- "$p"`, "psps-transfer", path)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return 0, "", transferErr(err, &errb)
	}
	lines := strings.Fields(out.String())
	if len(lines) < 2 {
		return 0, "", fmt.Errorf("reading %s: unexpected %q", path, out.String())
	}
	size, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil || len(lines[1]) != 64 {
		return 0, "", fmt.Errorf("reading %s: unexpected %q", path, out.String())
	}
	return size, lines[1], nil
}

// Transfer copies the file at path in container from into InboxDir/name in
// container to, part is its name there until it is whole. It reads at most
// max bytes, and the file must be size bytes with sha256 sum: otherwise
// nothing is left in the inbox.
func (c CLI) Transfer(ctx context.Context, from, path, to, name, part string, size int64, sum string) error {
	if err := TransferPath(path); err != nil {
		return err
	}
	if err := TransferName(name); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opts := ExecOpts{User: AgentUser, Env: []string{"HOME=" + AgentHome}}
	src := c.Exec(ctx, from, opts, "sh", "-c", transferCheck+`exec cat -- "$p"`, "psps-transfer", path)
	var srcErr, dstErr bytes.Buffer
	src.Stderr = &srcErr
	srcOut, err := src.StdoutPipe()
	if err != nil {
		return err
	}
	in := opts
	in.Interactive = true
	dst := c.Exec(ctx, to, in, "sh", "-c", `set -e
d=$1 n=$2 t=$1/$3
umask 022
mkdir -p "$d"
if [ -e "$d/$n" ] || [ -L "$d/$n" ]; then echo "$d/$n is there already" >&2; exit 17; fi
rm -f "$t"
cat > "$t"`, "psps-transfer", InboxDir, name, part)
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	dst.Stdin, dst.Stdout, dst.Stderr = r, &dstErr, &dstErr
	if err := dst.Start(); err != nil {
		r.Close()
		w.Close()
		return err
	}
	r.Close()
	if err := src.Start(); err != nil {
		w.Close()
		dst.Wait()
		return err
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(w, h), io.LimitReader(srcOut, size+1))
	w.Close()
	if cerr != nil || n > size {
		cancel()
	}
	io.Copy(io.Discard, srcOut)
	serr, derr := src.Wait(), dst.Wait()
	var ee *exec.ExitError
	switch {
	case derr != nil && errors.As(derr, &ee) && ee.ExitCode() == 17:
		return fmt.Errorf("%s/%s: %w", InboxDir, name, ErrTransferExists)
	case n > size:
		c.transferDrop(to, part)
		return fmt.Errorf("%s: %w: it was %d bytes, now over that", path, ErrTransferChanged, size)
	case serr != nil:
		c.transferDrop(to, part)
		return transferErr(serr, &srcErr)
	case derr != nil:
		c.transferDrop(to, part)
		return fmt.Errorf("writing it in the inbox: %w: %s", derr, strings.TrimSpace(dstErr.String()))
	case cerr != nil:
		c.transferDrop(to, part)
		return cerr
	case n != size || hex.EncodeToString(h.Sum(nil)) != sum:
		c.transferDrop(to, part)
		return fmt.Errorf("%s: %w: it was %d bytes with sha256 %s, now %d with %s", path, ErrTransferChanged, size, short(sum), n, short(hex.EncodeToString(h.Sum(nil))))
	}
	// Whole and as approved: a hard link gives it its name, and fails on
	// one taken meanwhile rather than replacing it.
	var out bytes.Buffer
	fin := c.Exec(ctx, to, opts, "sh", "-c", `d=$1 n=$2 t=$1/$3
if ln -- "$t" "$d/$n" 2>/dev/null; then rm -f -- "$t"; exit 0; fi
rm -f -- "$t"
if [ -e "$d/$n" ]; then exit 17; fi
exit 1`, "psps-transfer", InboxDir, name, part)
	fin.Stdout, fin.Stderr = &out, &out
	if err := fin.Run(); err != nil {
		if errors.As(err, &ee) && ee.ExitCode() == 17 {
			return fmt.Errorf("%s/%s: %w", InboxDir, name, ErrTransferExists)
		}
		return fmt.Errorf("naming it %s/%s: %w: %s", InboxDir, name, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// transferDrop removes what a transfer that failed left in the inbox.
func (c CLI) transferDrop(container, part string) {
	ctx, cancel := context.WithTimeout(context.Background(), transferDropTime)
	defer cancel()
	c.Exec(ctx, container, ExecOpts{User: AgentUser}, "rm", "-f", "--", InboxDir+"/"+part).Run()
}

const transferDropTime = 30 * time.Second

func transferErr(err error, stderr *bytes.Buffer) error {
	msg := strings.TrimSpace(stderr.String())
	var ee *exec.ExitError
	if errors.As(err, &ee) && (ee.ExitCode() == 3 || ee.ExitCode() == 4) && msg != "" {
		return errors.New(msg)
	}
	if msg != "" {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
