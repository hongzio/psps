// Package portproxy is port: a port of the isolation's, reachable from the
// Mac at 127.0.0.1:<host_port>, for a dev server or a WAS an agent runs.
// It is not a credential proxy; it holds nothing. The daemon listens on the
// Mac and connects what it accepts, through the isolation's tunnel, to the
// isolation's own loopback, so a server there that listens on localhost
// only is reached as well as one on every address.
//
// That is a hole in the isolation in the other direction from a share:
// what an agent serves reaches the Mac. It is drawn to be a small one:
//
//   - the Mac listens on 127.0.0.1 only, never the LAN, so only the Mac's
//     own programs connect. A port something on the Mac already listens
//     on is refused rather than shadowed: a program of the Mac's that
//     connects to its own server must not reach the isolation instead.
//   - nothing changes for the isolation's side: it still cannot connect to
//     the Mac (the fence measures that); a connection only ever starts on
//     the Mac.
//   - a browser sends the cookies of 127.0.0.1 to every port of it, gero's
//     own web session among them, whose value is the web token. In http
//     mode (the default) the daemon is an HTTP proxy that keeps gero's
//     cookies out of every request and out of every answer, so the
//     isolation never sees them and cannot replace them. tcp mode passes
//     bytes unchanged, for what is not HTTP; a browser pointed at a tcp
//     port hands the isolation the session.
//   - what an agent serves runs in the Mac's browser, as a page from
//     127.0.0.1, which browsers trust more than one from the internet. The
//     skill tells agents so; gero's own API refuses such a page (it needs a
//     header a cross-origin page cannot send).
//   - an http port answers only to a loopback name (127.0.0.1, localhost,
//     *.localhost): a page on the internet that points a name of its own at
//     127.0.0.1 (DNS rebinding) cannot read what the isolation serves
//     through the Mac's browser.
//
// Turning a port on or off changes only the Mac's listener: the container
// stays.
package portproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/store"
)

const Name = "port"

// Env is one forwarded port. HostPort 0 is the same as Port.
type Env struct {
	Port     int    `json:"port"`
	HostPort int    `json:"host_port,omitempty"`
	Mode     string `json:"mode"`
}

func (e Env) Forward(name string) proxy.Forward {
	host := e.HostPort
	if host == 0 {
		host = e.Port
	}
	return proxy.Forward{Env: name, Port: e.Port, HostPort: host, HTTP: e.Mode != "tcp"}
}

type Service struct{}

func (Service) Name() string { return Name }
func (Service) Ports() []int { return nil }
func (Service) Summary() string {
	return "a port of the isolation's (a dev server, a WAS) on the Mac's 127.0.0.1, never the LAN; http keeps your gero session from it"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "port", Kind: proxy.Int, Flag: "port", Metavar: "PORT", Help: "the port a server listens on in the isolation (required)"},
		{Key: "host_port", Kind: proxy.Int, Flag: "host-port", Metavar: "PORT", Help: "the port on the Mac's 127.0.0.1 (default, or 0: the same)"},
		{Key: "mode", Kind: proxy.String, Flag: "mode", Choices: []string{"http", "tcp"},
			Help: "http (default) keeps the browser's gero session from the isolation; tcp passes bytes unchanged, for what is not HTTP"},
	}
}

func (Service) Settings() []proxy.Field                { return nil }
func (Service) NormalizeSettings(map[string]any) error { return nil }

func (Service) Normalize(name string, env map[string]any) ([]string, error) {
	port := proxy.IntOr(env, "port", 0)
	switch {
	case port == 0:
		return nil, proxy.Deny("--port is required: the port a server listens on in the isolation")
	case port < 0 || port > 65535:
		return nil, proxy.Deny("--port %d is not a port", port)
	case port >= shimproto.TunnelPortLo && port <= shimproto.TunnelPortHi:
		return nil, proxy.Deny("--port %d is gero's own in the isolation (%d-%d are the credential proxies')", port, shimproto.TunnelPortLo, shimproto.TunnelPortHi)
	}
	host := proxy.IntOr(env, "host_port", 0)
	if host < 0 || host > 65535 {
		return nil, proxy.Deny("--host-port %d is not a port", host)
	}
	env["port"] = port
	if host == 0 || host == port {
		delete(env, "host_port")
	} else {
		env["host_port"] = host
	}
	mode := proxy.StringOr(env, "mode", "http")
	env["mode"] = mode
	if mode == "tcp" {
		if host == 0 {
			host = port
		}
		return []string{fmt.Sprintf("%s passes bytes unchanged: a browser that opens 127.0.0.1:%d sends it your gero web session "+
			"(a browser's cookies are not per port), which the isolation's agents can then read; use --mode http for anything a browser opens",
			name, host)}, nil
	}
	return nil, nil
}

// CheckExposure refuses two envs on one Mac port.
func (Service) CheckExposure(v proxy.View) error {
	fws, _ := Service{}.Forwards(v)
	by := map[int]string{}
	for _, f := range fws {
		if o, ok := by[f.HostPort]; ok {
			return proxy.Deny("%s and %s are both on the Mac's 127.0.0.1:%d; give one another --host-port", o, f.Env, f.HostPort)
		}
		by[f.HostPort] = f.Env
	}
	return nil
}

func (Service) Forwards(v proxy.View) (forwards []proxy.Forward, missing []string) {
	for _, name := range v.Exposure.Envs {
		var e Env
		if err := v.Decode(name, &e); err != nil {
			missing = append(missing, name+" ("+err.Error()+")")
			continue
		}
		forwards = append(forwards, e.Forward(name))
	}
	for _, name := range v.Missing {
		missing = append(missing, name+" (no such port env)")
	}
	return forwards, missing
}

// TokenKeys: none; nothing in the isolation asks the Mac for anything.
func (Service) TokenKeys(store.Exposure) []string { return nil }

// Workspace is the skill, which says which ports the Mac reaches.
func (s Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	fws, _ := s.Forwards(v)
	return proxy.Workspace{Files: proxy.SkillFiles(Name, skillBody(fws))}, nil
}

func (Service) Managed() []string { return proxy.SkillDirs(Name) }

// Check says what the env forwards; whether the Mac listens for it is the
// daemon's to say, per isolation.
func (Service) Check(_ context.Context, name string, cfg json.RawMessage, _ *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	return Describe(e.Forward(name)), nil
}

// Describe is a forward as people read it.
func Describe(f proxy.Forward) string {
	mode := "tcp"
	if f.HTTP {
		mode = "http"
	}
	return fmt.Sprintf("127.0.0.1:%d on the Mac → %d in the isolation (%s)", f.HostPort, f.Port, mode)
}

// Refusals: none it makes itself; `gero iso verify` measures the listeners.
func (Service) Refusals(proxy.View) []proxy.Refusal { return nil }

func (Service) New(proxy.Host) proxy.Instance { return noInstance{} }

type noInstance struct{}

func (noInstance) Serve(port int, l net.Listener) error {
	l.Close()
	return fmt.Errorf("port: no service on port %d", port)
}
