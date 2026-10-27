package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/client"
	"github.com/hongzio/gero/internal/paths"
	"github.com/hongzio/gero/internal/web"
)

// webCmd opens the web UI signed in, and says how to reach it from a phone.
// It never changes Tailscale's configuration itself: exposing the UI to the
// tailnet is the user's call, made with the command it prints.
func webCmd(p paths.Paths, args []string) error {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	printOnly := fs.Bool("url", false, "print the sign-in link instead of opening a browser")
	rotate := fs.Bool("rotate", false, "issue a new token, signing every browser out")
	fs.Parse(args)
	if err := ensureDaemon(p, false); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	h, err := client.New(p.Socket()).Health(ctx)
	cancel()
	if err != nil {
		return err
	}
	// Link only to the UI this daemon actually serves: with two daemons
	// (one in development), the port may belong to the other one, whose
	// token this one's link would not match.
	if h.Web == "" {
		return fmt.Errorf("this daemon (pid %d) is not serving the web UI: %s", h.PID, h.WebError)
	}
	var tok string
	if *rotate {
		tok, err = web.RotateToken(p.WebToken())
		fmt.Println("issued a new token; every browser has to sign in again")
	} else {
		tok, err = web.EnsureToken(p.WebToken())
	}
	if err != nil {
		return err
	}
	link := h.Web + "/login?token=" + url.QueryEscape(tok)
	if *printOnly {
		fmt.Println(link)
	} else if err := exec.Command("open", link).Run(); err != nil {
		fmt.Println("open this link:", link)
	} else {
		fmt.Println("opened the web UI at " + h.Web)
	}

	if host := tailnetName(); host != "" {
		port := h.Web[strings.LastIndex(h.Web, ":")+1:]
		fmt.Printf("\nFrom your phone, over Tailscale (HTTPS, tailnet devices only):\n")
		fmt.Printf("  once:  tailscale serve --bg %s\n", port)
		fmt.Printf("  then:  https://%s/login?token=%s\n", host, url.QueryEscape(tok))
		fmt.Println("  undo:  tailscale serve --https=443 off   (see `tailscale serve status`)")
	}
	return nil
}

// tailnetName is this Mac's MagicDNS name, or "" without Tailscale.
func tailnetName() string {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return ""
	}
	out, err := exec.Command(bin, "status", "--json").Output()
	if err != nil {
		return ""
	}
	var st struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if json.Unmarshal(out, &st) != nil {
		return ""
	}
	return strings.TrimSuffix(st.Self.DNSName, ".")
}
