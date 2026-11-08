package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hongzio/psps/internal/shim"
)

// serveHost is the shim on the Mac itself, for the admin agent: what
// `psps-shim serve` is in an isolation, run by the psps binary as the user,
// in a session of its own so the admin agent outlives the daemon. Agents get
// its own environment, which the daemon starts it with: the user's login
// shell's, so they find claude and codex where the user does.
func serveHost(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	env := []string{"TERM=dumb"}
	for _, kv := range os.Environ() {
		if len(kv) >= 5 && kv[:5] == "TERM=" {
			continue
		}
		env = append(env, kv)
	}
	srv := &shim.Server{Root: root, BaseEnv: env}
	if err := srv.Open(); err != nil {
		return err
	}
	sock := filepath.Join(root, "shim.sock")
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	os.Chmod(sock, 0o600)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	signal.Ignore(syscall.SIGHUP)
	stopping, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		<-sigs
		close(stopping)
		l.Close()
		srv.Shutdown(4 * time.Second)
		close(stopped)
	}()
	fmt.Fprintf(os.Stderr, "psps host shim %d: serving %s\n", os.Getpid(), sock)
	err = srv.Serve(l)
	select {
	case <-stopping:
		<-stopped
		return nil
	default:
		return err
	}
}
