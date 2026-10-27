package pgproxy

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// Ported from isolate's test_pgproxy.py. The SCRAM math is pinned to the
// RFC 7677 test vector, because a subtly wrong proof does not fail loudly:
// it fails as "password authentication failed" and sends the operator
// auditing the wrong secret. Everything past the math is measured on live
// sockets against a scripted upstream: what the proxy sends the real server
// (the stored password, the env's user, never the workspace token), and
// what it refuses before dialing upstream at all.

const (
	rfcUser        = "user"
	rfcPassword    = "pencil"
	rfcClientNonce = "rOprNGfwEbeRWgbNEkqO"
	rfcServerFirst = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	rfcClientFinal = "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	rfcServerSig   = "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
)

func TestClientFirstMessageMatchesTheRFCShape(t *testing.T) {
	full, bare := ScramClientFirst(rfcUser, rfcClientNonce)
	if full != "n,,n=user,r=rOprNGfwEbeRWgbNEkqO" || bare != "n=user,r=rOprNGfwEbeRWgbNEkqO" {
		t.Fatalf("%q %q", full, bare)
	}
}

func TestClientFinalReproducesTheRFCProofAndSignature(t *testing.T) {
	_, bare := ScramClientFirst(rfcUser, rfcClientNonce)
	final, sig, err := ScramClientFinal(rfcPassword, bare, rfcServerFirst)
	if err != nil || final != rfcClientFinal || sig != rfcServerSig {
		t.Fatalf("final=%q sig=%q err=%v", final, sig, err)
	}
}

func TestAServerNonceThatDropsOursIsRefused(t *testing.T) {
	// The combined nonce must extend the client's; a server that replaces
	// it is answering someone else's exchange (or replaying one).
	_, bare := ScramClientFirst(rfcUser, rfcClientNonce)
	tampered := strings.Replace(rfcServerFirst, rfcClientNonce, "somebodyelse", 1)
	if _, _, err := ScramClientFinal(rfcPassword, bare, tampered); err == nil {
		t.Fatal("accepted")
	}
}

func TestAMalformedServerFirstIsRefusedNotCrashed(t *testing.T) {
	_, bare := ScramClientFirst(rfcUser, rfcClientNonce)
	if _, _, err := ScramClientFinal(rfcPassword, bare, "r=abc,i=notanumber"); err == nil {
		t.Fatal("accepted")
	}
}

func TestStartupMessageRoundTrips(t *testing.T) {
	raw := startupMessage(map[string]string{"user": "dev", "database": "app"})
	if p := parseStartup(raw[8:]); len(p) != 2 || p["user"] != "dev" || p["database"] != "app" {
		t.Fatalf("%v", p)
	}
	if int(binary.BigEndian.Uint32(raw)) != len(raw) || binary.BigEndian.Uint32(raw[4:]) != 196608 {
		t.Fatalf("header %x", raw[:8])
	}
}

func TestErrorResponseCarriesSeverityCodeAndMessage(t *testing.T) {
	kind, payload, err := readMessage(bufio.NewReader(bytes.NewReader(errorResponse("no such environment", "08004"))), 0)
	f := parseError(payload)
	if err != nil || kind != 'E' || f["S"] != "ERROR" || f["C"] != "08004" || f["M"] != "no such environment" {
		t.Fatalf("%c %v %v", kind, f, err)
	}
}

func TestMessageFramingRoundTrips(t *testing.T) {
	kind, payload, err := readMessage(bufio.NewReader(bytes.NewReader(message('Q', []byte("SELECT 1\x00")))), 0)
	if err != nil || kind != 'Q' || string(payload) != "SELECT 1\x00" {
		t.Fatalf("%c %q %v", kind, payload, err)
	}
}

// --- policy ---

func policyView(envs map[string]Env) proxy.View {
	v := proxy.View{Isolation: "dev", Enabled: true, Envs: map[string]json.RawMessage{}, Tokens: map[string]string{"": "workspace-token"}}
	v.Exposure = store.Exposure{Isolation: "dev", Service: "pg", Default: "dev"}
	for n, e := range envs {
		b, _ := json.Marshal(e)
		v.Envs[n] = b
		v.Exposure.Envs = append(v.Exposure.Envs, n)
	}
	return v
}

var policyEnvs = map[string]Env{
	"dev":   {Host: "db.dev", Port: 5432, User: "ro", Databases: []string{"analytics", "app"}, SSLMode: "disable"},
	"empty": {Host: "db.dev", Port: 5432, User: "ro", SSLMode: "disable"},
}

func TestTheAllowlistIsEnvAndDatabase(t *testing.T) {
	in := &instance{}
	v := policyView(policyEnvs)
	if e, err := in.policy(v, "dev", "app"); err != nil || e.User != "ro" {
		t.Fatalf("an allowed pair: %v %v", e, err)
	}
	for _, c := range []struct{ env, db, want string }{
		{"prod", "app", "dev"},             // an unknown env names the known ones
		{"dev", "secrets", "app"},          // an unlisted database names the allowed
		{"empty", "app", "--allow-db"},     // an empty allowlist names the fix
		{"dev", "app\x00x", "not allowed"}, // nothing fancy gets through
	} {
		_, err := in.policy(v, c.env, c.db)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s/%s: %v", c.env, c.db, err)
		}
	}
}

// --- the proxy, on live sockets ---

// scramVerify is the server side of SCRAM, written independently of the
// code under test: the proof checks out, and the server signature follows.
func scramVerify(password, bare, serverFirst, clientFinal string) (string, bool) {
	withoutProof, proofB64, ok := strings.Cut(clientFinal, ",p=")
	if !ok {
		return "", false
	}
	fields := map[string]string{}
	for _, f := range strings.Split(serverFirst, ",") {
		k, v, _ := strings.Cut(f, "=")
		fields[k] = v
	}
	salt, _ := base64.StdEncoding.DecodeString(fields["s"])
	salted, _ := pbkdf2.Key(sha256.New, password, salt, 4096, 32)
	h := func(key []byte, s string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(s))
		return m.Sum(nil)
	}
	clientKey := h(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	auth := bare + "," + serverFirst + "," + withoutProof
	sig := h(stored[:], auth)
	proof, _ := base64.StdEncoding.DecodeString(proofB64)
	if len(proof) != len(sig) {
		return "", false
	}
	recovered := make([]byte, len(sig))
	for i := range sig {
		recovered[i] = proof[i] ^ sig[i]
	}
	if got := sha256.Sum256(recovered); got != stored {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(h(h(salted, "Server Key"), auth)), true
}

// fakeUpstream is a scripted Postgres server: real sockets, minimal
// protocol. It records what the proxy actually sent.
type fakeUpstream struct {
	auth, user, password string
	l                    net.Listener

	mu          sync.Mutex
	startups    []map[string]string
	passwords   []string
	cancels     [][]byte
	connections int
}

func newUpstream(t *testing.T, auth, password string) *fakeUpstream {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &fakeUpstream{auth: auth, user: "real_user", password: password, l: l}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			u.mu.Lock()
			u.connections++
			u.mu.Unlock()
			go u.serve(c)
		}
	}()
	return u
}

func (u *fakeUpstream) port() int { return u.l.Addr().(*net.TCPAddr).Port }

func (u *fakeUpstream) snapshot() (startups []map[string]string, passwords []string, cancels [][]byte, conns int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append(startups, u.startups...), append(passwords, u.passwords...), append(cancels, u.cancels...), u.connections
}

func (u *fakeUpstream) serve(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	for {
		var head [8]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return
		}
		n, code := binary.BigEndian.Uint32(head[:]), binary.BigEndian.Uint32(head[4:])
		if code == sslRequest {
			c.Write([]byte{'N'})
			continue
		}
		rest := make([]byte, n-8)
		io.ReadFull(r, rest)
		u.mu.Lock()
		if code == cancelRequest {
			u.cancels = append(u.cancels, rest)
			u.mu.Unlock()
			return
		}
		u.startups = append(u.startups, parseStartup(rest))
		u.mu.Unlock()
		break
	}
	if !u.authenticate(c, r) {
		return
	}
	var zero [4]byte
	c.Write(message('R', zero[:]))
	c.Write(message('S', []byte("server_version\x0016.3\x00")))
	key := make([]byte, 8)
	binary.BigEndian.PutUint32(key, 1234)
	binary.BigEndian.PutUint32(key[4:], 5678)
	c.Write(message('K', key))
	c.Write(message('Z', []byte("I")))
	for {
		kind, payload, err := readMessage(r, 0)
		if err != nil || kind == 'X' {
			return
		}
		if kind == 'Q' && bytes.HasPrefix(payload, []byte("SELECT datname")) {
			for _, row := range []string{"app", "postgres"} {
				d := make([]byte, 6)
				binary.BigEndian.PutUint16(d, 1)
				binary.BigEndian.PutUint32(d[2:], uint32(len(row)))
				c.Write(message('D', append(d, row...)))
			}
			c.Write(message('C', []byte("SELECT 2\x00")))
			c.Write(message('Z', []byte("I")))
		} else if kind == 'Q' {
			c.Write(message('C', []byte("SELECT 1\x00")))
			c.Write(message('Z', []byte("I")))
		}
	}
}

func authCode(code uint32, rest string) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, code)
	return message('R', append(b, rest...))
}

func (u *fakeUpstream) authenticate(c net.Conn, r *bufio.Reader) bool {
	refuse := func() bool {
		c.Write(errorResponse("password authentication failed for user \"real_user\"", "28P01"))
		return false
	}
	if u.auth == "cleartext" {
		c.Write(authCode(3, ""))
		_, payload, _ := readMessage(r, 0)
		offered := strings.TrimRight(string(payload), "\x00")
		u.mu.Lock()
		u.passwords = append(u.passwords, offered)
		u.mu.Unlock()
		return offered == u.password || refuse()
	}
	c.Write(authCode(10, "SCRAM-SHA-256\x00\x00"))
	_, payload, _ := readMessage(r, 0)
	mech, rest, _ := bytes.Cut(payload, []byte{0})
	if string(mech) != "SCRAM-SHA-256" || len(rest) < 4 {
		return refuse()
	}
	clientFirst := string(rest[4:])
	bare := strings.SplitN(clientFirst, ",", 3)[2]
	_, nonce, _ := strings.Cut(bare, "r=")
	serverFirst := "r=" + nonce + "SRVNONCE,s=" + base64.StdEncoding.EncodeToString([]byte("saltsaltsaltsalt")) + ",i=4096"
	c.Write(authCode(11, serverFirst))
	_, payload, _ = readMessage(r, 0)
	sig, ok := scramVerify(u.password, bare, serverFirst, string(payload))
	if !ok {
		return refuse()
	}
	if u.auth == "scram-forged" {
		// A server that never held the verifier cannot sign; this one tries.
		sig = base64.StdEncoding.EncodeToString(make([]byte, 32))
	}
	c.Write(authCode(12, "v="+sig))
	return true
}

const token = "workspace-token"

type harness struct {
	t    *testing.T
	up   *fakeUpstream
	port int

	mu     sync.Mutex
	view   proxy.View
	tokens map[string]string
}

func newHarness(t *testing.T, up *fakeUpstream) *harness {
	t.Helper()
	cs := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	if err := cs.Write("pg", "dev", map[string]string{"password": "real-pass"}); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, up: up}
	h.view = policyView(map[string]Env{"dev": {Host: "127.0.0.1", Port: up.port(), User: "real_user", Databases: []string{"app"}, SSLMode: "disable"}})
	in := Service{}.New(proxy.Host{Isolation: "dev", View: h.getView, Creds: cs, Log: log.New(io.Discard, "", 0)})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go in.Serve(Port, l)
	h.port = l.Addr().(*net.TCPAddr).Port
	return h
}

func (h *harness) getView() (proxy.View, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.view, nil
}

func (h *harness) edit(f func(v *proxy.View)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.view)
}

type client struct {
	c net.Conn
	r *bufio.Reader
}

func (h *harness) dial() *client {
	h.t.Helper()
	c, err := net.DialTimeout("tcp", h.l(), 5*time.Second)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	return &client{c, bufio.NewReader(c)}
}

func (h *harness) l() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(h.port)) }

// handshake speaks the isolation side up to ReadyForQuery or an error and
// returns the kinds that arrived after the token.
func (h *harness) handshake(user, database, tok string, sslProbe bool) (*client, []msg) {
	h.t.Helper()
	cl := h.dial()
	if sslProbe {
		var p [8]byte
		binary.BigEndian.PutUint32(p[:], 8)
		binary.BigEndian.PutUint32(p[4:], sslRequest)
		cl.c.Write(p[:])
		if b, _ := cl.r.ReadByte(); b != 'N' {
			h.t.Fatalf("SSL probe answered %q", b)
		}
	}
	cl.c.Write(startupMessage(map[string]string{"user": user, "database": database}))
	kind, payload, err := readMessage(cl.r, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	if kind == 'E' {
		return cl, []msg{{kind, payload}}
	}
	if kind != 'R' || binary.BigEndian.Uint32(payload) != 3 {
		h.t.Fatalf("expected a cleartext password request, got %c %x", kind, payload)
	}
	cl.c.Write(message('p', append([]byte(tok), 0)))
	var seen []msg
	for {
		kind, payload, err := readMessage(cl.r, 0)
		if err != nil {
			h.t.Fatalf("after %v: %v", seen, err)
		}
		seen = append(seen, msg{kind, payload})
		if kind == 'Z' || kind == 'E' {
			return cl, seen
		}
	}
}

func kinds(seen []msg) string {
	var b strings.Builder
	for _, m := range seen {
		b.WriteByte(m.kind)
	}
	return b.String()
}

func TestTheUpstreamGetsTheStoredPasswordNotTheToken(t *testing.T) {
	up := newUpstream(t, "cleartext", "real-pass")
	h := newHarness(t, up)
	_, seen := h.handshake("dev", "app", token, false)
	if kinds(seen)[0] != 'R' {
		t.Fatalf("seen %s", kinds(seen))
	}
	startups, passwords, _, _ := up.snapshot()
	if len(passwords) != 1 || passwords[0] != "real-pass" {
		t.Fatalf("the upstream got %v", passwords)
	}
	if startups[0]["user"] != "real_user" || startups[0]["database"] != "app" {
		t.Fatalf("startup %v", startups[0])
	}
}

func TestTheGreetingRelaysBackendKeyAndReady(t *testing.T) {
	h := newHarness(t, newUpstream(t, "cleartext", "real-pass"))
	_, seen := h.handshake("dev", "app", token, false)
	if k := kinds(seen); !strings.Contains(k, "K") || !strings.HasSuffix(k, "Z") {
		t.Fatalf("seen %s", k)
	}
}

func TestQueriesFlowBothWaysAfterTheHandshake(t *testing.T) {
	h := newHarness(t, newUpstream(t, "cleartext", "real-pass"))
	cl, _ := h.handshake("dev", "app", token, false)
	cl.c.Write(message('Q', []byte("SELECT 1\x00")))
	kind, payload, err := readMessage(cl.r, 0)
	if err != nil || kind != 'C' || string(payload) != "SELECT 1\x00" {
		t.Fatalf("%c %q %v", kind, payload, err)
	}
}

func TestAnSSLProbeGetsNThenProceeds(t *testing.T) {
	h := newHarness(t, newUpstream(t, "cleartext", "real-pass"))
	if _, seen := h.handshake("dev", "app", token, true); !strings.HasSuffix(kinds(seen), "Z") {
		t.Fatalf("seen %s", kinds(seen))
	}
}

func TestAWrongTokenIsRefusedBeforeUpstreamIsDialed(t *testing.T) {
	up := newUpstream(t, "cleartext", "real-pass")
	h := newHarness(t, up)
	for _, tok := range []string{"stolen", ""} {
		_, seen := h.handshake("dev", "app", tok, false)
		if kinds(seen) != "E" || parseError(seen[0].payload)["C"] != "28P01" {
			t.Fatalf("token %q: %s", tok, kinds(seen))
		}
	}
	if _, _, _, conns := up.snapshot(); conns != 0 {
		t.Fatal("upstream was dialed for a bad token")
	}
}

// What `gero iso verify` tries from inside the isolation with psql is what
// this proxy refuses, with the text verify looks for.
func TestVerifysRefusalIsRefused(t *testing.T) {
	up := newUpstream(t, "cleartext", "real-pass")
	h := newHarness(t, up)
	v, _ := h.getView()
	rs := Service{}.Refusals(v)
	if len(rs) != 1 || rs[0].Postgres != "dev" || rs[0].Port != Port {
		t.Fatalf("%+v", rs)
	}
	_, seen := h.handshake(rs[0].Postgres, rs[0].Postgres, proxy.WrongToken, true)
	if kinds(seen) != "E" || !strings.Contains(parseError(seen[0].payload)["M"], rs[0].Want) {
		t.Fatalf("%s %q", kinds(seen), parseError(seen[0].payload)["M"])
	}
}

func TestAnIsolationWithNoTokenMintedRefusesAnEmptyPassword(t *testing.T) {
	up := newUpstream(t, "cleartext", "real-pass")
	h := newHarness(t, up)
	h.edit(func(v *proxy.View) { v.Tokens = map[string]string{} })
	if _, seen := h.handshake("dev", "app", "", false); kinds(seen) != "E" {
		t.Fatalf("seen %s", kinds(seen))
	}
}

func TestADatabaseOffTheAllowlistNeverReachesUpstream(t *testing.T) {
	up := newUpstream(t, "cleartext", "real-pass")
	h := newHarness(t, up)
	_, seen := h.handshake("dev", "secrets", token, false)
	if kinds(seen) != "E" || !strings.Contains(parseError(seen[0].payload)["M"], "app") {
		t.Fatalf("seen %s %v", kinds(seen), parseError(seen[0].payload))
	}
	if _, _, _, conns := up.snapshot(); conns != 0 {
		t.Fatal("upstream was dialed")
	}
}

func TestScramUpstreamAuthenticatesEndToEnd(t *testing.T) {
	h := newHarness(t, newUpstream(t, "scram", "real-pass"))
	_, seen := h.handshake("dev", "app", token, false)
	if k := kinds(seen); k[0] != 'R' || !strings.HasSuffix(k, "Z") {
		t.Fatalf("seen %s %v", k, parseError(seen[len(seen)-1].payload))
	}
}

func TestAServerSignatureThatDoesNotVerifyIsRefused(t *testing.T) {
	// Whatever answered accepted our proof but cannot prove it holds the
	// verifier: queries must not go to it.
	h := newHarness(t, newUpstream(t, "scram-forged", "real-pass"))
	_, seen := h.handshake("dev", "app", token, false)
	if kinds(seen) != "E" || !strings.Contains(parseError(seen[0].payload)["M"], "server signature") {
		t.Fatalf("seen %s %v", kinds(seen), parseError(seen[len(seen)-1].payload))
	}
}

func TestACancelRequestReachesTheRightUpstream(t *testing.T) {
	up := newUpstream(t, "cleartext", "real-pass")
	h := newHarness(t, up)
	_, seen := h.handshake("dev", "app", token, false)
	var key []byte
	for _, m := range seen {
		if m.kind == 'K' {
			key = m.payload
		}
	}
	cl := h.dial()
	pkt := make([]byte, 8)
	binary.BigEndian.PutUint32(pkt, 16)
	binary.BigEndian.PutUint32(pkt[4:], cancelRequest)
	cl.c.Write(append(pkt, key...))
	cl.c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, cancels, _ := up.snapshot(); len(cancels) == 1 {
			if want := []byte{0, 0, 4, 210, 0, 0, 22, 46}; !bytes.Equal(cancels[0], want) {
				t.Fatalf("cancel = %v", cancels[0])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the cancel never reached upstream")
}

func TestABadUpstreamPasswordRelaysTheServersRefusal(t *testing.T) {
	h := newHarness(t, newUpstream(t, "cleartext", "different"))
	_, seen := h.handshake("dev", "app", token, false)
	if kinds(seen) != "E" || !strings.Contains(parseError(seen[0].payload)["M"], "password") {
		t.Fatalf("seen %s", kinds(seen))
	}
}

func TestAMissingSecretNamesTheFix(t *testing.T) {
	up := newUpstream(t, "cleartext", "real-pass")
	h := newHarness(t, up)
	h.edit(func(v *proxy.View) {
		b, _ := json.Marshal(Env{Host: "127.0.0.1", Port: up.port(), User: "real_user", Databases: []string{"app"}, SSLMode: "disable"})
		v.Envs["ghost"] = b
		v.Exposure.Envs = append(v.Exposure.Envs, "ghost")
	})
	_, seen := h.handshake("ghost", "app", token, false)
	if kinds(seen) != "E" || !strings.Contains(parseError(seen[0].payload)["M"], "gero env secret pg ghost") {
		t.Fatalf("%v", parseError(seen[0].payload))
	}
}

func TestAnIsolationWithoutTheServiceIsToldHowToGetIt(t *testing.T) {
	h := newHarness(t, newUpstream(t, "cleartext", "real-pass"))
	h.edit(func(v *proxy.View) { v.Enabled = false })
	cl := h.dial()
	cl.c.Write(startupMessage(map[string]string{"user": "dev", "database": "app"}))
	kind, payload, _ := readMessage(cl.r, 0)
	if kind != 'E' || !strings.Contains(parseError(payload)["M"], "gero iso expose dev pg") {
		t.Fatalf("%c %v", kind, parseError(payload))
	}
}

func TestReplicationIsRefused(t *testing.T) {
	up := newUpstream(t, "cleartext", "real-pass")
	h := newHarness(t, up)
	cl := h.dial()
	cl.c.Write(startupMessage(map[string]string{"user": "dev", "database": "app", "replication": "database"}))
	readMessage(cl.r, 0) // the password request
	cl.c.Write(message('p', append([]byte(token), 0)))
	kind, payload, _ := readMessage(cl.r, 0)
	if kind != 'E' || !strings.Contains(parseError(payload)["M"], "replication") {
		t.Fatalf("%c %v", kind, parseError(payload))
	}
}

func TestAnOversizedPasswordMessageIsRefusedUnread(t *testing.T) {
	h := newHarness(t, newUpstream(t, "cleartext", "real-pass"))
	cl := h.dial()
	cl.c.Write(startupMessage(map[string]string{"user": "dev", "database": "app"}))
	readMessage(cl.r, 0)
	head := []byte{'p', 0x7f, 0xff, 0xff, 0xff}
	cl.c.Write(head)
	kind, payload, _ := readMessage(cl.r, 0)
	if kind != 'E' || parseError(payload)["C"] != "28P01" {
		t.Fatalf("%c %v", kind, parseError(payload))
	}
}

// --- `gero env check pg` exercises the same client path, from the Mac ---

func TestCheckEnvReportsTheServerVersion(t *testing.T) {
	up := newUpstream(t, "scram", "real-pass")
	e := Env{Host: "127.0.0.1", Port: up.port(), User: "real_user", Databases: []string{"app"}, SSLMode: "disable"}
	res, err := CheckEnv(t.Context(), "dev", e, "real-pass")
	if err != nil || res.ServerVersion != "16.3" || res.Database != "app" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestListDatabasesReadsThemOffARealExchange(t *testing.T) {
	up := newUpstream(t, "cleartext", "real-pass")
	e := Env{Host: "127.0.0.1", Port: up.port(), User: "real_user", Databases: []string{"app"}, SSLMode: "disable"}
	names, err := ListDatabases(t.Context(), e, "real-pass")
	if err != nil || strings.Join(names, ",") != "app,postgres" {
		t.Fatalf("%v %v", names, err)
	}
}

func TestCheckNamesAMissingSecret(t *testing.T) {
	up := newUpstream(t, "cleartext", "real-pass")
	cfg, _ := json.Marshal(Env{Host: "127.0.0.1", Port: up.port(), User: "real_user", Databases: []string{"app"}, SSLMode: "disable"})
	_, err := Service{}.Check(t.Context(), "ghost", cfg, &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	if err == nil || !strings.Contains(err.Error(), "[pg ghost]") {
		t.Fatalf("%v", err)
	}
}

func TestMD5AuthenticationMatchesPostgres(t *testing.T) {
	// 'md5' || md5(md5(password || user) || salt), the vector computed
	// with Python's hashlib as isolate's code does.
	if got := md5Password("postgres", "secret", []byte{1, 2, 3, 4}); got != "md5bb41a296aab6baccb36ff243a562abff" {
		t.Fatalf("%q", got)
	}
}
