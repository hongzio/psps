package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/shim"
	"github.com/hongzio/gero/internal/shimproto"
)

// A SIGTERM — what `container stop` sends — must let agents exit and log it.
// The bug this pins: serve returned as soon as the listener closed, and the
// process ended before Shutdown had stopped anyone.
func TestSIGTERMLogsAgentExits(t *testing.T) {
	root, err := os.MkdirTemp("", "gss")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)

	done := make(chan error, 1)
	go func() { done <- serve([]string{"--root", root}) }()

	c := &shim.Client{Dial: shim.UnixDialer(filepath.Join(root, "shim.sock"))}
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := c.Ping(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shim did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := c.Start(ctx, "a1", shimproto.Spec{Argv: []string{"/bin/sleep", "60"}}); err != nil {
		t.Fatal(err)
	}

	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after SIGTERM")
	}

	b, _ := os.ReadFile(filepath.Join(root, "agents", "a1", "log.jsonl"))
	if !strings.Contains(string(b), `"kind":"exited"`) {
		t.Fatalf("no exit logged after SIGTERM:\n%s", b)
	}
}
