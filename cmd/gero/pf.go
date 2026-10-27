package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/pf"
)

// pfCmd manages the network fence's root helper. install and uninstall run
// under sudo; status needs nothing.
func pfCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: sudo gero pf install | sudo gero pf uninstall | gero pf status")
	}
	switch args[0] {
	case "install":
		uid, _ := strconv.Atoi(os.Getenv("SUDO_UID"))
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := pf.Install(exe, uid); err != nil {
			return err
		}
		fmt.Printf("network fence helper installed (%s) for uid %d\n", pf.HelperLabel, uid)
		fmt.Println("it keeps pf on and loads the rules into the com.apple/gero anchor; /etc/pf.conf is not modified")
		fmt.Println("remove it with: sudo gero pf uninstall")
		return nil
	case "uninstall":
		if err := pf.Uninstall(); err != nil {
			return err
		}
		fmt.Println("network fence helper removed; isolations with the fence on will not start agents until it is back")
		return nil
	case "status":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, err := pf.Client{Socket: pf.SocketPath}.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("pf enabled: %v\nnetworks:   %d\n", st.Enabled, len(st.Networks))
		for _, n := range st.Networks {
			fmt.Printf("  %s  %s via %s\n", n.Name, n.Subnet4, n.Gateway4)
		}
		fmt.Printf("rules:      %d loaded in %s", len(st.Loaded), pf.Anchor)
		if !st.AppliedAt.IsZero() {
			fmt.Printf(" (applied %s)", st.AppliedAt.Local().Format(time.RFC3339))
		}
		fmt.Println()
		if st.LastError != "" {
			fmt.Println("last error:", st.LastError)
		}
		if len(args) > 1 && args[1] == "-v" {
			fmt.Println("  " + strings.Join(st.Loaded, "\n  "))
		}
		return nil
	}
	return fmt.Errorf("unknown pf command %q", args[0])
}

// pfdCmd is what the LaunchDaemon runs, as root.
func pfdCmd(args []string) error {
	if len(args) != 3 || args[0] != "serve" || args[1] != "--uid" {
		return errors.New("usage: gero pfd serve --uid N (run by launchd)")
	}
	uid, err := strconv.Atoi(args[2])
	if err != nil || uid <= 0 {
		return fmt.Errorf("bad uid %q", args[2])
	}
	if os.Geteuid() != 0 {
		return errors.New("the fence helper must run as root")
	}
	ctx, cancel := signalContext()
	defer cancel()
	return pf.NewHelper(uid, log.New(os.Stderr, "", log.LstdFlags)).Serve(ctx)
}
