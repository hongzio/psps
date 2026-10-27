package pgproxy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/store"
)

const (
	Port = 3022
	// PassFile holds the workspace token as libpq's password, agent-owned
	// and 0600 because libpq ignores a pgpass anyone else can read.
	PassFile = "/etc/gero/pgpass"

	handshakeTimeout = 30 * time.Second
	shutdownGrace    = 10 * time.Second
)

// DBName is conservative on purpose, like the AWS patterns: a database name
// that needs quoting has no business in an allowlist an ErrorResponse will
// echo back.
var DBName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_$-]{0,62}$`)

var SSLModes = []string{"disable", "prefer", "require", "verify-full"}

type Service struct{}

func (Service) Name() string { return "pg" }
func (Service) Ports() []int { return []int{Port} }
func (Service) Summary() string {
	return "PostgreSQL; the proxy signs in with the stored password, an allowlist of databases"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "host", Kind: proxy.String, Flag: "host", Metavar: "HOST", Help: "the PostgreSQL server (required)"},
		{Key: "port", Kind: proxy.Int, Flag: "port", Metavar: "PORT", Help: "its port (default 5432)"},
		{Key: "user", Kind: proxy.String, Flag: "user", Metavar: "ROLE",
			Help: "the role the proxy signs in as (required); its grants are the boundary, so pick a narrow one"},
		{Key: "sslmode", Kind: proxy.String, Flag: "sslmode", Metavar: "MODE", Choices: SSLModes,
			Help: "TLS to the server, as libpq means it (default prefer)"},
		{Key: "databases", Kind: proxy.List, Flag: "allow-db", Clear: "clear-dbs", Metavar: "NAME",
			Help: "a database that opens (repeat; replaces the list)"},
	}
}

func (Service) Settings() []proxy.Field                { return nil }
func (Service) NormalizeSettings(map[string]any) error { return nil }

func (Service) Normalize(name string, env map[string]any) ([]string, error) {
	dbs := proxy.Strings(env, "databases")
	for _, d := range dbs {
		if !DBName.MatchString(d) {
			return nil, fmt.Errorf("%q is not a usable database name (letters, digits, _ - $)", d)
		}
	}
	env["databases"] = proxy.SortedSet(dbs)
	if proxy.StringOr(env, "host", "") == "" {
		return nil, fmt.Errorf("--host is required; it names the PostgreSQL server %q opens", name)
	}
	if proxy.StringOr(env, "user", "") == "" {
		return nil, fmt.Errorf("--user is required; it names the role the proxy signs in as — that role's grants are the boundary, so pick a narrow one")
	}
	port := proxy.IntOr(env, "port", 5432)
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("--port %d is not a TCP port", port)
	}
	env["port"] = port
	env["sslmode"] = proxy.StringOr(env, "sslmode", "prefer")
	var warn []string
	if len(dbs) == 0 {
		warn = append(warn, fmt.Sprintf("env %q allows no databases; the proxy refuses every connection until --allow-db", name))
	}
	if env["sslmode"] != "verify-full" {
		warn = append(warn, fmt.Sprintf("env %q: sslmode=%s does not verify the server's certificate; verify-full does", name, env["sslmode"]))
	}
	return warn, nil
}

func (Service) TokenKeys(store.Exposure) []string { return []string{""} }

func decodeEnvs(v proxy.View) (map[string]Env, error) {
	envs := map[string]Env{}
	for _, name := range v.Exposure.Envs {
		if _, ok := v.Envs[name]; !ok {
			continue
		}
		var e Env
		if err := v.Decode(name, &e); err != nil {
			return nil, fmt.Errorf("pg env %s: %w", name, err)
		}
		envs[name] = e
	}
	return envs, nil
}

// Workspace plants libpq's standard variables and a pgpass holding the
// workspace token: psql, psycopg and every libpq binding read them, so a
// bare `psql` works. The user names an environment; the real role and
// password stay on the Mac.
func (Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	envs, err := decodeEnvs(v)
	if err != nil {
		return proxy.Workspace{}, err
	}
	ws := proxy.Workspace{
		Env: []string{"PGHOST=127.0.0.1", fmt.Sprintf("PGPORT=%d", Port), "PGUSER=" + v.Exposure.Default, "PGPASSFILE=" + PassFile},
		Files: []shimproto.SyncFile{{Path: PassFile, Mode: 0o600, Agent: true,
			Content: fmt.Sprintf("127.0.0.1:%d:*:*:%s\n", Port, v.Tokens[""])}},
	}
	if e, ok := envs[v.Exposure.Default]; ok && len(e.Databases) > 0 {
		ws.Env = append(ws.Env, "PGDATABASE="+e.Databases[0])
	}
	ws.Files = append(ws.Files, proxy.SkillFiles("pg", skillBody(v, envs))...)
	return ws, nil
}

func (Service) Managed() []string { return append([]string{PassFile}, proxy.SkillDirs("pg")...) }

func (Service) New(h proxy.Host) proxy.Instance {
	return &instance{h: h, keys: map[[8]byte]Env{}}
}

type instance struct {
	h proxy.Host

	mu   sync.Mutex
	keys map[[8]byte]Env // live sessions' BackendKeyData, for CancelRequest
}

func (in *instance) logf(format string, a ...any) { in.h.Logf("pg", format, a...) }

func (in *instance) Serve(port int, l net.Listener) error {
	if port != Port {
		return fmt.Errorf("pg proxy: no service on port %d", port)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go in.handle(c)
	}
}

func (in *instance) refuse(c net.Conn, err error) {
	d, ok := err.(*Denied)
	if !ok {
		d = &Denied{Reason: err.Error(), Code: "08004"}
	}
	c.Write(errorResponse(d.Reason, d.Code))
	in.logf("DENY %s", d.Reason)
}

func (in *instance) handle(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	r := bufio.NewReaderSize(c, 64<<10)
	params, err := in.startup(c, r)
	if err != nil {
		in.refuse(c, err)
		return
	}
	if params == nil {
		return
	}
	envName := params["user"]
	database := params["database"]
	if database == "" {
		database = envName
	}
	v, err := in.h.View()
	if err != nil {
		in.refuse(c, err)
		return
	}
	if !v.Enabled {
		in.refuse(c, deny("the pg proxy is off for isolation %s; the operator turns it on with `gero iso expose %s pg <env>`", in.h.Isolation, in.h.Isolation))
		return
	}
	if err := in.checkToken(c, r, v, envName); err != nil {
		in.refuse(c, err)
		return
	}
	if _, ok := params["replication"]; ok {
		in.refuse(c, deny("replication connections are not proxied; the allowlist models databases and replication streams every one of them"))
		return
	}
	e, err := in.policy(v, envName, database)
	if err != nil {
		in.refuse(c, err)
		return
	}
	secret, err := in.h.Creds.Read("pg", envName)
	if err != nil {
		in.refuse(c, deny("%s", err.Error()))
		return
	}
	up, err := openUpstream(context.Background(), e, database, secret["password"], params)
	if err != nil {
		in.refuse(c, err)
		return
	}
	defer up.conn.Close()
	if up.key != nil {
		in.mu.Lock()
		in.keys[*up.key] = e
		in.mu.Unlock()
		defer func() {
			in.mu.Lock()
			delete(in.keys, *up.key)
			in.mu.Unlock()
		}()
	}
	var ok [4]byte // AuthenticationOk
	out := message('R', ok[:])
	for _, m := range up.greeting {
		out = append(out, message(m.kind, m.payload)...)
	}
	if _, err := c.Write(out); err != nil {
		return
	}
	if up.startErr() != nil {
		in.logf("FAIL env=%s db=%s: upstream errored during start", envName, database)
		return
	}
	in.logf("OPEN env=%s db=%s -> %s as %s", envName, database, e.addr(), e.User)
	c.SetDeadline(time.Time{})
	// Either side may end first. Once the server's side has, the client gets
	// a grace period to finish, and the deferred closes end the rest: a
	// tunnel connection has no deadlines to do it with.
	outbound := make(chan struct{})
	go func() {
		pump(up.conn, r)
		close(outbound)
	}()
	pump(c, up.r)
	select {
	case <-outbound:
	case <-time.After(shutdownGrace):
	}
	in.logf("CLOSE env=%s db=%s", envName, database)
}

// pump is one direction of the splice; it half-closes the destination at
// EOF so the peer sees the end of the stream rather than a reset.
func pump(dst net.Conn, src io.Reader) {
	io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		dst.Close()
	}
}

// startup reads the first packet, the only unframed one in the protocol.
// SSL and GSS probes get 'N': the isolation's leg is loopback carried by
// the tunnel, and a TLS story there would be theater; the client falls back
// to plaintext as libpq documents. A CancelRequest is a whole connection in
// itself; it is forwarded and done (nil params, nil error).
func (in *instance) startup(c net.Conn, r *bufio.Reader) (map[string]string, error) {
	for {
		var head [8]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return nil, nil
		}
		length := int(int32(binary.BigEndian.Uint32(head[:])))
		code := binary.BigEndian.Uint32(head[4:])
		if length < 8 || length > maxStartup {
			return nil, deny("startup packet length %d is not a Postgres client's", length)
		}
		if code == sslRequest || code == gssencRequest {
			if _, err := c.Write([]byte{'N'}); err != nil {
				return nil, nil
			}
			continue
		}
		rest := make([]byte, length-8)
		if _, err := io.ReadFull(r, rest); err != nil {
			return nil, nil
		}
		if code == cancelRequest {
			in.cancel(rest)
			return nil, nil
		}
		if code != protocol30 {
			return nil, deny("only PostgreSQL protocol 3.0 is spoken here (startup code %d)", code)
		}
		return parseStartup(rest), nil
	}
}

// checkToken has the isolation prove itself with its workspace token as the
// password, before any policy answer: which environments exist and what
// they allow is nobody's business until then.
func (in *instance) checkToken(c net.Conn, r *bufio.Reader, v proxy.View, envName string) error {
	var three [4]byte
	binary.BigEndian.PutUint32(three[:], 3)
	if _, err := c.Write(message('R', three[:])); err != nil {
		return err
	}
	kind, payload, err := readMessage(r, maxStartup)
	if err != nil || kind != 'p' {
		return &Denied{Reason: "expected the workspace token as a password message", Code: "28P01"}
	}
	offered := strings.TrimRight(string(payload), "\x00")
	want := v.Tokens[""]
	if want == "" || subtle.ConstantTimeCompare([]byte(offered), []byte(want)) != 1 {
		in.logf("DENY env=%s: bad workspace token", envName)
		return &Denied{Reason: "missing or wrong workspace token; this proxy serves the isolation's PGPASSFILE only", Code: "28P01"}
	}
	return nil
}

// policy is which (environment, database) pairs open at all. The
// environment rides in the startup packet's user field, the one slot a
// connection string always carries, so the isolation picks dev or prod by
// name, with the real role supplied here, never by the caller.
func (in *instance) policy(v proxy.View, envName, database string) (Env, error) {
	envs, err := decodeEnvs(v)
	if err != nil {
		return Env{}, err
	}
	e, ok := envs[envName]
	if !ok {
		for _, m := range v.Missing {
			if m == envName {
				return Env{}, deny("environment %q was removed from gero's registry but is still exposed here; the operator re-exposes with `gero iso expose %s pg ...`", envName, v.Isolation)
			}
		}
		return Env{}, deny("no environment named %q here; this proxy serves: %s. Connect with the environment name as the user (postgres://<env>@...)", envName, names(envs))
	}
	if len(e.Databases) == 0 {
		return Env{}, deny("environment %q allows no databases; the operator must add one with `gero env edit pg %s --allow-db <name>`", envName, envName)
	}
	for _, d := range e.Databases {
		if d == database {
			return e, nil
		}
	}
	return Env{}, deny("database %q is not allowed for environment %q; allowed: %s", database, envName, strings.Join(e.Databases, ", "))
}

func names(envs map[string]Env) string {
	l := make([]string, 0, len(envs))
	for n := range envs {
		l = append(l, n)
	}
	sort.Strings(l)
	if len(l) == 0 {
		return "(none)"
	}
	return strings.Join(l, ", ")
}

// cancel forwards a CancelRequest to the upstream its key belongs to,
// best-effort, which is the protocol's own contract for cancellation.
func (in *instance) cancel(payload []byte) {
	if len(payload) != 8 {
		return
	}
	key := [8]byte(payload)
	in.mu.Lock()
	e, ok := in.keys[key]
	in.mu.Unlock()
	if !ok {
		in.logf("CANCEL unknown backend key; dropped")
		return
	}
	raw, err := net.DialTimeout("tcp", e.addr(), upstreamTimeout)
	if err != nil {
		in.logf("CANCEL failed: %v", err)
		return
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(upstreamTimeout))
	conn, err := negotiateTLS(raw, e)
	if err != nil {
		in.logf("CANCEL failed: %v", err)
		return
	}
	pkt := make([]byte, 8, 16)
	binary.BigEndian.PutUint32(pkt, 16)
	binary.BigEndian.PutUint32(pkt[4:], cancelRequest)
	conn.Write(append(pkt, key[:]...))
	in.logf("CANCEL forwarded to %s", e.Host)
}

// Refusals: a wrong workspace token as the password, for the env the
// isolation reaches by default.
func (Service) Refusals(v proxy.View) []proxy.Refusal {
	user := v.Exposure.Default
	if user == "" {
		user = "gero-verify"
	}
	return []proxy.Refusal{{What: "a wrong workspace token", Port: Port, Postgres: user, Want: "missing or wrong workspace token"}}
}

// Check connects from the Mac, now, with the stored password.
func (Service) Check(ctx context.Context, name string, cfg json.RawMessage, c *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	if len(e.Databases) == 0 {
		return "", deny("no databases allowed yet; add one with `gero env edit pg %s --allow-db <name>` first", name)
	}
	secret, err := c.Read("pg", name)
	if err != nil {
		return "", err
	}
	res, err := CheckEnv(ctx, name, e, secret["password"])
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("server     %s, PostgreSQL %s\nconnected  %s as %s\ndatabases  %s\n",
		e.addr(), res.ServerVersion, res.Database, e.User, strings.Join(e.Databases, ", ")), nil
}
