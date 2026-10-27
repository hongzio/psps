// Package launchd registers the daemon as a per-user LaunchAgent, so it comes
// back after a crash (KeepAlive) and after a reboot (at login). It is
// per-user, not a system daemon, because Apple's container runtime is too.
package launchd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hongzio/gero/internal/paths"
)

const Label = "dev.gero.daemon"

// launchd starts jobs with /usr/bin:/bin:/usr/sbin:/sbin, which has no
// Homebrew; `container` and git live there.
const jobPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

func PlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func esc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func render(exe string, p paths.Paths) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + esc(exe) + `</string>
		<string>daemon</string>
		<string>run</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key><string>` + jobPath + `</string>
	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>10</integer>
	<key>StandardOutPath</key><string>` + esc(p.Log()) + `</string>
	<key>StandardErrorPath</key><string>` + esc(p.Log()) + `</string>
</dict>
</plist>
`)
	return []byte(b.String())
}

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func Loaded() bool {
	_, err := launchctl("print", domain()+"/"+Label)
	return err == nil
}

// Ensure registers the job for exe and starts it. A job registered for a
// different binary is replaced; one already registered and current is only
// kicked.
func Ensure(exe string, p paths.Paths) error {
	if err := p.Ensure(); err != nil {
		return err
	}
	want := render(exe, p)
	have, _ := os.ReadFile(PlistPath())
	loaded := Loaded()
	if !bytes.Equal(want, have) {
		if loaded {
			launchctl("bootout", domain()+"/"+Label)
			loaded = false
		}
		if err := os.MkdirAll(filepath.Dir(PlistPath()), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(PlistPath(), want, 0o644); err != nil {
			return err
		}
	}
	if !loaded {
		if out, err := launchctl("bootstrap", domain(), PlistPath()); err != nil {
			return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
		}
		return nil
	}
	if out, err := launchctl("kickstart", domain()+"/"+Label); err != nil {
		return fmt.Errorf("launchctl kickstart: %v: %s", err, out)
	}
	return nil
}

// Stop unloads the job; KeepAlive would otherwise restart a killed daemon.
func Stop() error {
	if !Loaded() {
		return nil
	}
	if out, err := launchctl("bootout", domain()+"/"+Label); err != nil {
		return fmt.Errorf("launchctl bootout: %v: %s", err, out)
	}
	return nil
}

func Uninstall() error {
	if err := Stop(); err != nil {
		return err
	}
	if err := os.Remove(PlistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
