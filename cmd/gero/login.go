package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/client"
)

// browserLogin runs a service's browser login. The daemon holds the
// exchange; this side only gets the address the browser lands on back to
// it, by whichever way arrives first: a browser on this Mac reaches the
// loopback listener by itself, and from any other device the operator
// pastes the address of the page that would not load.
func browserLogin(ctx context.Context, c *client.Client, service, name string, in io.Reader, out io.Writer) error {
	got := make(chan string, 2)
	redirect := ""
	if r, caught, stop, err := client.CatchRedirect(); err == nil {
		defer stop()
		redirect = r
		go func() {
			if q, ok := <-caught; ok {
				got <- q
			}
		}()
	}
	login, err := c.StartLogin(ctx, service, name, redirect)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Log in to %s in a browser and approve the access:\n\n  %s\n\n", service, login.URL)
	fmt.Fprintf(out, "A browser on this Mac comes back here by itself. On any other device the page it\n"+
		"lands on afterwards (%s?...) will not load: copy that page's address and paste it here.\n\n", login.Redirect)
	go func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 64<<10), 64<<10)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				got <- line
				return
			}
		}
	}()
	var callback string
	select {
	case callback = <-got:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Until(login.Expires)):
		return fmt.Errorf("no login came back before it expired; run `gero env secret %s %s` again", service, name)
	}
	if err := c.FinishLogin(ctx, service, name, login.ID, callback); err != nil {
		return err
	}
	fmt.Fprintf(out, "logged in; [%s %s] is stored and proxies use it on their next request\n", service, name)
	return nil
}
