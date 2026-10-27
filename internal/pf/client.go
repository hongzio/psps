package pf

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
)

// Client is the user daemon's side of the helper's socket.
type Client struct {
	Socket string
	// Owner files this daemon's networks separately from any other's.
	Owner string
}

var ErrNotInstalled = errors.New("the network fence helper is not installed; run `sudo gero pf install`")

func (c Client) Installed() bool {
	_, err := os.Stat(c.Socket)
	return err == nil
}

func (c Client) call(ctx context.Context, req Request) (Status, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		if !c.Installed() {
			return Status{}, ErrNotInstalled
		}
		return Status{}, fmt.Errorf("network fence helper: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return Status{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return Status{}, fmt.Errorf("network fence helper: %w", err)
	}
	var s Status
	if err := json.Unmarshal(line, &s); err != nil {
		return Status{}, err
	}
	if s.Error != "" {
		return s, errors.New("network fence helper: " + s.Error)
	}
	return s, nil
}

func (c Client) Status(ctx context.Context) (Status, error) {
	return c.call(ctx, Request{Op: "status"})
}

// Apply replaces the set of fenced networks and returns the helper's state
// after reloading.
func (c Client) Apply(ctx context.Context, networks []Network) (Status, error) {
	if networks == nil {
		networks = []Network{}
	}
	return c.call(ctx, Request{Op: "apply", Owner: c.Owner, Networks: networks})
}
