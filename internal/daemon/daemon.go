// Package daemon is gero's long-running host process: it owns the store,
// reconciles isolations and agents toward it, and serves the API.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/paths"
	"github.com/hongzio/gero/internal/pf"
	"github.com/hongzio/gero/internal/services"
	"github.com/hongzio/gero/internal/store"
	"github.com/hongzio/gero/internal/web"
)

var ErrAlreadyRunning = errors.New("another gero daemon is already running")

// Run serves until ctx ends. A second daemon finds the lock held and returns
// ErrAlreadyRunning without touching the first one's socket.
func Run(ctx context.Context, p paths.Paths, logger *log.Logger) error {
	if err := p.Ensure(); err != nil {
		return err
	}
	lock, err := os.OpenFile(p.Lock(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrAlreadyRunning
	}
	lock.Truncate(0)
	fmt.Fprintf(lock, "%d\n", os.Getpid())

	cli, err := isolation.FindCLI()
	if err != nil {
		return err
	}
	st, err := store.Open(p.DB())
	if err != nil {
		return err
	}
	defer st.Close()

	// Holding the lock makes any socket file a stale one.
	os.Remove(p.Socket())
	l, err := net.Listen("unix", p.Socket())
	if err != nil {
		return err
	}
	os.Chmod(p.Socket(), 0o600)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The helper fences every daemon's networks; this one's are filed under
	// its state directory, so a second daemon (a dev one) cannot drop them.
	m := NewManager(ctx, st, cli, pf.Client{Socket: pf.SocketPath, Owner: p.State}, logger)
	m.SetProxies(services.Registry(), &creds.Store{Path: p.Credentials()})
	if err := m.migrateEnvAccounts(); err != nil {
		logger.Printf("moving tokens from isolation environments into accounts: %v", err)
	}
	go func() {
		if err := m.Reconcile(); err != nil {
			logger.Printf("reconcile: %v", err)
		}
	}()

	srv := &http.Server{Handler: m.Handler(), ReadHeaderTimeout: 10 * time.Second}
	webSrv, err := startWeb(p, m, logger)
	if err != nil {
		m.webErr = err.Error()
		logger.Printf("web UI not served: %v", err)
	}
	go func() {
		<-ctx.Done()
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		srv.Shutdown(sctx)
		if webSrv != nil {
			webSrv.Shutdown(sctx)
		}
	}()
	logger.Printf("gero daemon %d serving %s", os.Getpid(), p.Socket())
	err = srv.Serve(l)
	cancel()
	m.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// WebAddr is where the web UI listens: loopback only. GERO_WEB_ADDR moves it
// (still meant to be loopback) or, set to "off", turns it off. A development
// daemon (GERO_HOME) defaults to another port, so it never takes the real
// daemon's.
func WebAddr() string {
	if a := os.Getenv("GERO_WEB_ADDR"); a != "" {
		return a
	}
	if os.Getenv("GERO_HOME") != "" {
		return "127.0.0.1:4377"
	}
	return "127.0.0.1:4376"
}

func startWeb(p paths.Paths, m *Manager, logger *log.Logger) (*http.Server, error) {
	addr := WebAddr()
	if addr == "off" {
		m.webErr = "turned off (GERO_WEB_ADDR=off)"
		return nil, nil
	}
	if _, err := web.EnsureToken(p.WebToken()); err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ws := &web.Server{API: m.Handler(), TokenFile: p.WebToken()}
	srv := &http.Server{Handler: ws.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("web UI: %v", err)
		}
	}()
	m.webURL = "http://" + l.Addr().String()
	logger.Printf("web UI on %s", m.webURL)
	return srv, nil
}
