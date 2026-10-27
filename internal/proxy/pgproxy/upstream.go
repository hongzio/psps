package pgproxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

const upstreamTimeout = 30 * time.Second

// Env is one PostgreSQL environment: a server, the role the proxy signs in
// as, and the databases that open. That role's grants are the boundary
// inside them.
type Env struct {
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	User      string   `json:"user"`
	SSLMode   string   `json:"sslmode"`
	Databases []string `json:"databases"`
}

func (e Env) addr() string {
	port := e.Port
	if port == 0 {
		port = 5432
	}
	return net.JoinHostPort(e.Host, strconv.Itoa(port))
}

// negotiateTLS is the client side of Postgres TLS: ask, then wrap or walk
// away. verify-full checks the certificate against the system store and the
// host name; require and prefer encrypt without verifying, which matches
// libpq's own defaults for those modes rather than pretending to more.
func negotiateTLS(conn net.Conn, e Env) (net.Conn, error) {
	mode := e.SSLMode
	if mode == "" {
		mode = "prefer"
	}
	if mode == "disable" {
		return conn, nil
	}
	var req [8]byte
	binary.BigEndian.PutUint32(req[:], 8)
	binary.BigEndian.PutUint32(req[4:], sslRequest)
	if _, err := conn.Write(req[:]); err != nil {
		return nil, err
	}
	var answer [1]byte
	if _, err := io.ReadFull(conn, answer[:]); err != nil {
		return nil, err
	}
	if answer[0] == 'S' {
		cfg := &tls.Config{ServerName: e.Host}
		if mode != "verify-full" {
			cfg.InsecureSkipVerify = true
		}
		tc := tls.Client(conn, cfg)
		if err := tc.Handshake(); err != nil {
			return nil, err
		}
		return tc, nil
	}
	if mode == "prefer" {
		return conn, nil
	}
	return nil, deny("upstream %s refused TLS and sslmode=%s requires it", e.addr(), mode)
}

// authenticate answers the server's requests until AuthenticationOk.
// SCRAM's final step verifies the server's signature too: a wrong v= means
// whatever answered does not hold the credential's verifier, and the
// connection is refused rather than trusted on transport alone.
func authenticate(conn net.Conn, r *bufio.Reader, e Env, password string) error {
	var bare, serverSig string
	started := false
	for {
		kind, payload, err := readMessage(r, 0)
		if err != nil {
			return err
		}
		if kind == 'E' {
			msg := parseError(payload)["M"]
			if msg == "" {
				msg = "no message"
			}
			return deny("upstream refused authentication: %s", msg)
		}
		if kind != 'R' || len(payload) < 4 {
			return deny("upstream sent %q before authentication completed", kind)
		}
		switch code := binary.BigEndian.Uint32(payload); code {
		case 0:
			return nil
		case 3:
			_, err = conn.Write(message('p', append([]byte(password), 0)))
		case 5:
			if len(payload) < 8 {
				return deny("upstream sent a short MD5 salt")
			}
			_, err = conn.Write(message('p', append([]byte(md5Password(e.User, password, payload[4:8])), 0)))
		case 10:
			var mechs []string
			for _, m := range strings.Split(string(payload[4:]), "\x00") {
				if m != "" {
					mechs = append(mechs, m)
				}
			}
			found := false
			for _, m := range mechs {
				found = found || m == "SCRAM-SHA-256"
			}
			if !found {
				offer := strings.Join(mechs, ", ")
				if offer == "" {
					offer = "no SASL mechanism"
				}
				return deny("upstream offers %s; only SCRAM-SHA-256 is spoken here", offer)
			}
			nonce := make([]byte, 18)
			rand.Read(nonce)
			var full string
			full, bare = ScramClientFirst("", base64.RawURLEncoding.EncodeToString(nonce))
			started = true
			body := append([]byte("SCRAM-SHA-256\x00"), 0, 0, 0, 0)
			binary.BigEndian.PutUint32(body[len(body)-4:], uint32(len(full)))
			_, err = conn.Write(message('p', append(body, full...)))
		case 11:
			if !started {
				return deny("upstream continued a SASL exchange that never started")
			}
			var final string
			if final, serverSig, err = ScramClientFinal(password, bare, string(payload[4:])); err != nil {
				return err
			}
			_, err = conn.Write(message('p', []byte(final)))
		case 12:
			if !started || serverSig == "" {
				return deny("upstream finished a SASL exchange that never started")
			}
			if string(payload[4:]) != "v="+serverSig {
				return deny("the upstream's SCRAM server signature does not verify; refusing to trust it with queries")
			}
		default:
			return deny("upstream requests authentication method %d, which this proxy does not speak", code)
		}
		if err != nil {
			return err
		}
	}
}

type msg struct {
	kind    byte
	payload []byte
}

// session is a live, authenticated upstream connection. greeting is every
// message after AuthenticationOk up to ReadyForQuery (or an error), relayed
// to the isolation verbatim.
type session struct {
	conn     net.Conn
	r        *bufio.Reader
	greeting []msg
	key      *[8]byte
}

// openUpstream dials, negotiates TLS and authenticates. params are the
// client's startup parameters, forwarded except for the ones the proxy
// decides.
func openUpstream(ctx context.Context, e Env, database, password string, params map[string]string) (*session, error) {
	d := net.Dialer{Timeout: upstreamTimeout}
	raw, err := d.DialContext(ctx, "tcp", e.addr())
	if err != nil {
		return nil, deny("cannot reach upstream %s: %v", e.addr(), err)
	}
	s, err := startSession(raw, e, database, password, params)
	if err != nil {
		raw.Close()
		if _, ok := err.(*Denied); ok {
			return nil, err
		}
		return nil, deny("upstream %s failed during session start: %v", e.addr(), err)
	}
	return s, nil
}

func startSession(raw net.Conn, e Env, database, password string, params map[string]string) (*session, error) {
	raw.SetDeadline(time.Now().Add(upstreamTimeout))
	conn, err := negotiateTLS(raw, e)
	if err != nil {
		return nil, err
	}
	startup := map[string]string{"user": e.User, "database": database}
	for k, v := range params {
		if k != "user" && k != "database" && k != "replication" {
			startup[k] = v
		}
	}
	if _, err := conn.Write(startupMessage(startup)); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(conn, 64<<10)
	if err := authenticate(conn, r, e, password); err != nil {
		return nil, err
	}
	s := &session{conn: conn, r: r}
	for {
		kind, payload, err := readMessage(r, 0)
		if err == errClosed {
			return nil, deny("upstream %s closed during session start", e.addr())
		}
		if err != nil {
			return nil, err
		}
		s.greeting = append(s.greeting, msg{kind, payload})
		if kind == 'K' && len(payload) == 8 {
			s.key = (*[8]byte)(payload)
		}
		if kind == 'Z' || kind == 'E' {
			raw.SetDeadline(time.Time{})
			return s, nil
		}
	}
}

func (s *session) close() {
	s.conn.Write(message('X', nil))
	s.conn.Close()
}

// startErr is the server's error during session start, if it sent one.
func (s *session) startErr() error {
	if n := len(s.greeting); n > 0 && s.greeting[n-1].kind == 'E' {
		m := parseError(s.greeting[n-1].payload)["M"]
		if m == "" {
			m = "upstream error"
		}
		return deny("%s", m)
	}
	return nil
}

// CheckResult is what `gero env check pg` reports.
type CheckResult struct {
	Database      string
	ServerVersion string
}

// CheckEnv connects from the Mac, now, as the proxy would: a wrong
// password, a firewalled port or a demoted role is found by the operator at
// the terminal, not by the agent as an opaque ErrorResponse.
func CheckEnv(ctx context.Context, name string, e Env, password string) (CheckResult, error) {
	if len(e.Databases) == 0 {
		return CheckResult{}, deny("no databases allowed yet; add one with `gero env edit pg %s --allow-db <name>` first", name)
	}
	dbs := append([]string(nil), e.Databases...)
	sort.Strings(dbs)
	s, err := openUpstream(ctx, e, dbs[0], password, nil)
	if err != nil {
		return CheckResult{}, err
	}
	defer s.close()
	if err := s.startErr(); err != nil {
		return CheckResult{}, err
	}
	res := CheckResult{Database: dbs[0], ServerVersion: "?"}
	for _, m := range s.greeting {
		if m.kind == 'S' {
			if k, v, ok := strings.Cut(string(m.payload), "\x00"); ok && k == "server_version" {
				res.ServerVersion = strings.TrimRight(v, "\x00")
			}
		}
	}
	return res, nil
}

// ListDatabases is every database the server admits connections to, from
// the server: a completion source, never a policy input. The maintenance
// database is tried first, then wherever the allowlist already reaches,
// then the role's own name (libpq's default), because a locked-down server
// may bar the role from postgres itself.
func ListDatabases(ctx context.Context, e Env, password string) ([]string, error) {
	cands := append([]string{"postgres"}, e.Databases...)
	sort.Strings(cands[1:])
	cands = append(cands, e.User)
	var last error
	tried := map[string]bool{}
	for _, db := range cands {
		if tried[db] {
			continue
		}
		tried[db] = true
		names, err := listOn(ctx, e, db, password)
		if err == nil {
			return names, nil
		}
		last = err
	}
	if last == nil {
		last = deny("no database answered a listing attempt")
	}
	return nil, last
}

func listOn(ctx context.Context, e Env, db, password string) ([]string, error) {
	s, err := openUpstream(ctx, e, db, password, nil)
	if err != nil {
		return nil, err
	}
	defer s.close()
	if err := s.startErr(); err != nil {
		return nil, err
	}
	s.conn.SetDeadline(time.Now().Add(upstreamTimeout))
	if _, err := s.conn.Write(message('Q', []byte("SELECT datname FROM pg_database WHERE datallowconn ORDER BY datname\x00"))); err != nil {
		return nil, err
	}
	names := []string{}
	for {
		kind, payload, err := readMessage(s.r, 0)
		if err != nil {
			return nil, fmt.Errorf("listing databases failed: %w", err)
		}
		switch kind {
		case 'D':
			if len(payload) >= 6 && binary.BigEndian.Uint16(payload) >= 1 {
				n := int(int32(binary.BigEndian.Uint32(payload[2:])))
				if n >= 0 && 6+n <= len(payload) {
					names = append(names, string(payload[6:6+n]))
				}
			}
		case 'E':
			m := parseError(payload)["M"]
			if m == "" {
				m = "listing databases failed"
			}
			return nil, deny("%s", m)
		case 'Z':
			return names, nil
		}
	}
}
