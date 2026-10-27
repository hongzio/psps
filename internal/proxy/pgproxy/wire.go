// Package pgproxy is the authenticating PostgreSQL proxy, a port of
// isolate's pgproxy.py: the isolation gets a database, never a password.
//
// The git proxy injects a header and the AWS broker issues scoped
// credentials; neither shape fits Postgres, whose credential lives inside
// its own wire protocol's handshake. So this proxy terminates that handshake
// on both sides, the way pgbouncer does:
//
//	isolation  psql postgres://<env>@127.0.0.1:3022/<db>   (password: the
//	             |  workspace token, from PGPASSFILE)
//	daemon     this proxy: checks the token, checks the (env, database)
//	             allowlist, then opens ITS OWN connection to the real server
//	             and completes SCRAM-SHA-256 with the stored password
//	             |  after that, a byte pump; no per-message parsing
//	           the real PostgreSQL (TLS originated here, per env sslmode)
//
// This works because the proxy is a legitimate client, not a man in the
// middle: it holds the real password and runs its own exchange. The
// password comes from the credentials file ([pg <env>]) per connection, so
// rotation is picked up at once and no second copy exists.
//
// The allowlist is (environment, database), and that is deliberately all of
// it. What the connection may SELECT or ALTER is enforced by Postgres
// through the grants on the env's user. A read-only mode judged here by
// parsing SQL would be a mode flag pretending to be a boundary: the
// extended protocol, DO blocks and functions all walk around a statement
// filter.
//
// Refusals are protocol-level ErrorResponses carrying their reason. One
// departure from a plain pump: the greeting is relayed message by message so
// BackendKeyData can be recorded, because a CancelRequest arrives on its own
// connection carrying only that key.
package pgproxy

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const (
	protocol30    = 196608
	sslRequest    = 80877103
	gssencRequest = 80877104
	cancelRequest = 80877102

	// A startup packet is a handful of short strings. A length beyond this
	// is not a Postgres client; refusing it caps what an unauthenticated
	// caller can make the daemon buffer. The token's password message gets
	// the same cap.
	maxStartup = 10 * 1024
)

// Denied is a refusal with the SQLSTATE the client sees it under.
type Denied struct {
	Reason string
	Code   string
}

func (d *Denied) Error() string { return d.Reason }

func deny(format string, a ...any) *Denied {
	return &Denied{Reason: fmt.Sprintf(format, a...), Code: "08004"}
}

// --- SCRAM-SHA-256 (RFC 5802/7677), the client half -------------------------

// ScramClientFirst returns the full and bare client-first messages.
// Postgres ignores n= (the user comes from the startup packet), but the RFC
// shape keeps the math checkable against its published test vector.
func ScramClientFirst(user, nonce string) (full, bare string) {
	bare = "n=" + user + ",r=" + nonce
	return "n,," + bare, bare
}

// ScramClientFinal returns the client-final message and the server
// signature to expect. The nonce check is the exchange's whole point: a
// server-first whose nonce does not extend ours is answering some other
// session, and computing a proof for it would sign the wrong transcript.
func ScramClientFinal(password, clientFirstBare, serverFirst string) (final, serverSignature string, err error) {
	fields := map[string]string{}
	for _, part := range strings.Split(serverFirst, ",") {
		if k, v, ok := strings.Cut(part, "="); ok {
			if _, seen := fields[k]; !seen {
				fields[k] = v
			}
		}
	}
	combined, okR := fields["r"]
	salt, errS := base64.StdEncoding.DecodeString(fields["s"])
	iterations, errI := strconv.Atoi(fields["i"])
	if !okR || errS != nil || errI != nil || iterations <= 0 {
		return "", "", deny("malformed SCRAM server-first message (%q)", serverFirst)
	}
	_, clientNonce, _ := strings.Cut(clientFirstBare, ",r=")
	if !strings.HasPrefix(combined, clientNonce) {
		return "", "", deny("the server's SCRAM nonce does not extend ours; refusing to answer what may be a replayed exchange")
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", "", deny("SCRAM key derivation failed: %v", err)
	}
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	clientKey := mac(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	withoutProof := "c=" + base64.StdEncoding.EncodeToString([]byte("n,,")) + ",r=" + combined
	authMessage := clientFirstBare + "," + serverFirst + "," + withoutProof
	signature := mac(storedKey[:], authMessage)
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ signature[i]
	}
	serverKey := mac(salted, "Server Key")
	return withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof),
		base64.StdEncoding.EncodeToString(mac(serverKey, authMessage)), nil
}

func md5Password(user, password string, salt []byte) string {
	inner := md5.Sum([]byte(password + user))
	outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), salt...))
	return "md5" + hex.EncodeToString(outer[:])
}

// --- framing ------------------------------------------------------------------

func message(kind byte, payload []byte) []byte {
	out := make([]byte, 5, 5+len(payload))
	out[0] = kind
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)+4))
	return append(out, payload...)
}

// errClosed is a clean close between messages.
var errClosed = errors.New("connection closed")

// readMessage reads one typed message, refusing a body over limit (0 for
// none: the upstream is trusted to frame sensibly).
func readMessage(r *bufio.Reader, limit int) (byte, []byte, error) {
	kind, err := r.ReadByte()
	if err != nil {
		return 0, nil, errClosed
	}
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, errors.New("connection closed inside a message header")
	}
	n := int(int32(binary.BigEndian.Uint32(head[:])))
	if n < 4 || (limit > 0 && n-4 > limit) {
		return 0, nil, fmt.Errorf("nonsense message length %d", n)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, errors.New("connection closed inside a message body")
	}
	return kind, payload, nil
}

// startupMessage frames params with user and database first, the rest in
// name order.
func startupMessage(params map[string]string) []byte {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k != "user" && k != "database" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range []string{"database", "user"} {
		if _, ok := params[k]; ok {
			keys = append([]string{k}, keys...)
		}
	}
	var body bytes.Buffer
	for _, k := range keys {
		body.WriteString(k)
		body.WriteByte(0)
		body.WriteString(params[k])
		body.WriteByte(0)
	}
	body.WriteByte(0)
	out := make([]byte, 8, 8+body.Len())
	binary.BigEndian.PutUint32(out, uint32(body.Len()+8))
	binary.BigEndian.PutUint32(out[4:], protocol30)
	return append(out, body.Bytes()...)
}

func parseStartup(payload []byte) map[string]string {
	parts := bytes.Split(payload, []byte{0})
	params := map[string]string{}
	for i := 0; i+1 < len(parts); i += 2 {
		if len(parts[i]) == 0 {
			break
		}
		params[string(parts[i])] = string(parts[i+1])
	}
	return params
}

func errorResponse(reason, code string) []byte {
	var b bytes.Buffer
	b.WriteString("SERROR\x00VERROR\x00C" + code + "\x00M" + reason + "\x00")
	b.WriteByte(0)
	return message('E', b.Bytes())
}

func parseError(payload []byte) map[string]string {
	fields := map[string]string{}
	for _, part := range bytes.Split(payload, []byte{0}) {
		if len(part) > 0 {
			fields[string(part[:1])] = string(part[1:])
		}
	}
	return fields
}
