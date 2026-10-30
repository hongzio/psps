package pf

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Install puts the helper in place for uid and starts it. It must run as
// root. The helper is a root-owned copy of the gero binary: a LaunchDaemon
// pointing at a file the user can write would hand root to anything running
// as that user.
func Install(exe string, uid int) error {
	if os.Geteuid() != 0 {
		return errors.New("installing the network fence needs root: run it with sudo")
	}
	if uid <= 0 {
		return errors.New("cannot tell which user the fence is for; run it with sudo from that user's shell")
	}
	if err := os.MkdirAll(filepath.Dir(HelperPath), 0o755); err != nil {
		return err
	}
	if err := copyRootOwned(exe, HelperPath, 0o755); err != nil {
		return err
	}
	plist := renderPlist(uid)
	if err := writeRootOwned(HelperPlist, plist, 0o644); err != nil {
		return err
	}
	if err := reload(HelperPlist, 10*time.Second); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(SocketPath); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the helper did not start; see %s", LogPath)
}

// launchctl runs launchctl; tests stand in for it.
var launchctl = func(args ...string) ([]byte, error) {
	return exec.Command("launchctl", args...).CombinedOutput()
}

// reload replaces the loaded helper with plist's. bootout returns before
// launchd has let go of the label, and a bootstrap in that window fails
// with 5 (Input/output error) — every reinstall over a running helper did,
// leaving none running. So it waits for the label to be gone, and still
// retries a bootstrap that fails, until wait runs out.
func reload(plist string, wait time.Duration) error {
	launchctl("bootout", "system/"+HelperLabel)
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if _, err := launchctl("print", "system/"+HelperLabel); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for {
		out, err := launchctl("bootstrap", "system", plist)
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Uninstall stops the helper and removes the fence and everything Install
// created. pf itself stays on if anything else holds a reference to it.
func Uninstall() error {
	if os.Geteuid() != 0 {
		return errors.New("removing the network fence needs root: run it with sudo")
	}
	exec.Command("launchctl", "bootout", "system/"+HelperLabel).Run()
	err := Teardown(execRunner{}, StateDir)
	for _, p := range []string{HelperPlist, HelperPath, SocketPath} {
		if rerr := os.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
			err = errors.Join(err, rerr)
		}
	}
	return errors.Join(err, os.RemoveAll(StateDir))
}

func esc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func renderPlist(uid int) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + HelperLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + esc(HelperPath) + `</string>
		<string>pfd</string>
		<string>serve</string>
		<string>--uid</string>
		<string>` + strconv.Itoa(uid) + `</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>5</integer>
	<key>StandardOutPath</key><string>` + LogPath + `</string>
	<key>StandardErrorPath</key><string>` + LogPath + `</string>
</dict>
</plist>
`)
}

func copyRootOwned(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, in); err != nil {
		return err
	}
	return writeRootOwned(dst, buf.Bytes(), mode)
}

// writeRootOwned writes via a temp file and rename, so a running helper is
// never replaced by a half-written binary.
func writeRootOwned(dst string, data []byte, mode os.FileMode) error {
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chown(tmp, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
