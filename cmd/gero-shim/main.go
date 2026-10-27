// gero-shim runs inside an isolation container.
//
//	gero-shim serve    the container's main process: owns agents, keeps logs
//	gero-shim client   relays stdio to the serve socket; the daemon runs this
//	                   through `container exec -i` so it needs no IP route
//	gero-shim probe    tries TCP targets and a DNS lookup, for the daemon to
//	                   measure the network fence from inside the isolation
//	gero-shim tunnel   the isolation end of the credential proxies' tunnel,
//	                   an SSH server on stdio (see internal/tunnel)
//	gero-shim sync     writes the files gero manages in the isolation, from
//	                   a JSON request on stdin
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hongzio/gero/internal/shim"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/tunnel"
)

const defaultRoot = "/var/lib/gero"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gero-shim serve|client|probe|tunnel|sync|version [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "client":
		err = client(os.Args[2:])
	case "probe":
		err = probe(os.Args[2:])
	case "tunnel":
		err = tunnelCmd(os.Args[2:])
	case "sync":
		err = syncCmd(os.Args[2:])
	case "version":
		fmt.Println(shimproto.Version)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gero-shim:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	root := fs.String("root", defaultRoot, "durable state directory")
	uid := fs.Int("uid", -1, "run agents as this uid")
	gid := fs.Int("gid", -1, "run agents as this gid")
	user := fs.String("user", "agent", "USER for agents")
	home := fs.String("home", "/home/agent", "HOME for agents")
	fs.Parse(args)

	// The root may be a volume mount point that already exists as 0755;
	// agents run as another user and must not read each other's logs.
	if err := os.MkdirAll(*root, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(*root, 0o700); err != nil {
		return err
	}
	srv := &shim.Server{
		Root: *root,
		BaseEnv: []string{
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"HOME=" + *home,
			"USER=" + *user,
			"LANG=C.UTF-8",
			"TERM=dumb",
		},
	}
	if *uid >= 0 {
		srv.Cred = &syscall.Credential{Uid: uint32(*uid), Gid: uint32(*gid)}
		// A fresh named volume mounted over the home directory is owned by
		// root; the agent user could not write its own config into it.
		if err := os.MkdirAll(filepath.Join(*home, "work"), 0o755); err != nil {
			return err
		}
		for _, p := range []string{*home, filepath.Join(*home, "work")} {
			if err := os.Chown(p, *uid, *gid); err != nil {
				return err
			}
		}
	}
	if err := srv.Open(); err != nil {
		return err
	}

	sock := filepath.Join(*root, "shim.sock")
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	os.Chmod(sock, 0o600)

	// `container stop` sends SIGTERM and kills after 5s by default. Agents
	// get 4 of those to exit, so their exits are logged as exits rather than
	// found as "lost" on the next boot.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	stopping, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		<-sigs
		close(stopping)
		l.Close()
		srv.Shutdown(4 * time.Second)
		close(stopped)
	}()
	fmt.Fprintf(os.Stderr, "gero-shim: serving %s\n", sock)
	err = srv.Serve(l)
	select {
	case <-stopping:
		// Serve returned because the listener closed; returning now would
		// exit before the agents do.
		<-stopped
		return nil
	default:
		return err
	}
}

func client(args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	root := fs.String("root", defaultRoot, "durable state directory")
	fs.Parse(args)

	conn, err := net.Dial("unix", filepath.Join(*root, "shim.sock"))
	if err != nil {
		return err
	}
	uc := conn.(*net.UnixConn)
	go func() {
		io.Copy(uc, os.Stdin)
		uc.CloseWrite()
	}()
	_, err = io.Copy(os.Stdout, uc)
	return err
}

// probe reports, as JSON, what each TCP target does from here: "open",
// "blocked" (no answer: dropped), "refused" (an answer: reachable, port
// closed), or another error. Only "blocked" means the packet never arrived.
func probe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	timeout := fs.Duration("timeout", 2*time.Second, "per-target timeout")
	resolve := fs.String("resolve", "", "a name to look up through the container's resolver")
	fs.Parse(args)

	type result struct {
		Resolve string            `json:"resolve,omitempty"`
		TCP     map[string]string `json:"tcp"`
	}
	r := result{TCP: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, target := range fs.Args() {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			outcome := "open"
			conn, err := net.DialTimeout("tcp", target, *timeout)
			// Right after boot an IPv6 link-local source address is still
			// tentative and no packet can be sent; wait that out rather
			// than report a measurement that never happened.
			for settle := time.Now().Add(3 * time.Second); errors.Is(err, syscall.EADDRNOTAVAIL) && time.Now().Before(settle); {
				time.Sleep(200 * time.Millisecond)
				conn, err = net.DialTimeout("tcp", target, *timeout)
			}
			var nerr net.Error
			switch {
			case err == nil:
				conn.Close()
			case errors.As(err, &nerr) && nerr.Timeout():
				outcome = "blocked"
			case strings.Contains(err.Error(), "refused"):
				outcome = "refused"
			default:
				outcome = "error: " + err.Error()
			}
			mu.Lock()
			r.TCP[target] = outcome
			mu.Unlock()
		}(target)
	}
	if *resolve != "" {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		if _, err := net.DefaultResolver.LookupHost(ctx, *resolve); err != nil {
			r.Resolve = err.Error()
		} else {
			r.Resolve = "ok"
		}
	}
	wg.Wait()
	return json.NewEncoder(os.Stdout).Encode(r)
}

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error {
	os.Stdout.Close()
	return os.Stdin.Close()
}

// tunnelCmd serves the daemon's end of the proxy tunnel on stdio until the
// daemon goes away. It runs as root so an agent cannot end it or take its
// ports.
func tunnelCmd(args []string) error {
	fs := flag.NewFlagSet("tunnel", flag.ExitOnError)
	pidfile := fs.String("pidfile", "/run/gero-tunnel.pid", "where the running tunnel's pid is kept")
	fs.Parse(args)
	if err := shim.TakeOver(*pidfile, "tunnel"); err != nil {
		return err
	}
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "gero-shim tunnel: "+format+"\n", a...) }
	err := tunnel.Serve(stdio{}, tunnel.LoopbackPorts(shimproto.TunnelPortLo, shimproto.TunnelPortHi), logf)
	if b, rerr := os.ReadFile(*pidfile); rerr == nil && strings.TrimSpace(string(b)) == fmt.Sprint(os.Getpid()) {
		os.Remove(*pidfile)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func syncCmd(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	uid := fs.Int("uid", 1000, "owner of the agent's files")
	gid := fs.Int("gid", 1000, "group of the agent's files")
	fs.Parse(args)
	var req shimproto.SyncRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		return fmt.Errorf("sync: bad request: %w", err)
	}
	res, err := shim.Sync(req, *uid, *gid)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}
