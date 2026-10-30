package linearproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/creds"
)

const (
	DefaultIssuer = "https://mcp.linear.app"
	MCPPath       = "/mcp"
	// Scope is asked for whole: writes are opened by the tool table, so a
	// family opened later needs no new login.
	Scope = "read write"
	// ProtocolVersion is what the check claims.
	ProtocolVersion = "2025-06-18"
	// refreshMargin: refresh this long before the stated expiry, so a
	// request never leaves with a token that dies in flight.
	refreshMargin = 60 * time.Second
)

// oauth is the client side of Linear's MCP authorization server.
type oauth struct {
	issuer    string
	transport http.RoundTripper
	now       func() time.Time
}

// resource is RFC 8707's: the token is asked for the MCP endpoint by name,
// so it is good for that and nothing else Linear runs.
func (o oauth) resource() string { return o.issuer + MCPPath }

func (o oauth) client() *http.Client {
	return &http.Client{Transport: o.transport, Timeout: 30 * time.Second}
}

func (o oauth) post(ctx context.Context, path, contentType string, body []byte) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", o.issuer+path, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var answer map[string]any
	json.Unmarshal(raw, &answer)
	return resp.StatusCode, answer, nil
}

func (o oauth) tokenRequest(ctx context.Context, form url.Values, env string) (map[string]any, error) {
	status, answer, err := o.post(ctx, "/token", "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return nil, deny(502, "linear: the token endpoint is unreachable: %v", err)
	}
	if tok, _ := answer["access_token"].(string); status == 200 && tok != "" {
		return answer, nil
	}
	code, _ := answer["error"].(string)
	if code == "invalid_grant" || status == 400 || status == 401 {
		if code == "" {
			code = fmt.Sprintf("HTTP %d", status)
		}
		return nil, deny(503, "linear: the Linear login in [linear %s] has expired or was revoked (%s); run `gero env secret linear %s` to log in again", env, code, env)
	}
	if code == "" {
		code = "no error code"
	}
	return nil, deny(502, "linear: the token endpoint answered HTTP %d (%s)", status, code)
}

// refreshLocks serialise refreshes per credentials file and env, across
// every isolation sharing the login: a rotated refresh token spent twice is
// a login lost. The daemon is the only process that refreshes or logs in.
var refreshLocks sync.Map

func refreshLock(c *creds.Store, env string) *sync.Mutex {
	l, _ := refreshLocks.LoadOrStore(c.Path+"\x00"+env, &sync.Mutex{})
	return l.(*sync.Mutex)
}

func readLogin(c *creds.Store, env string) (map[string]string, error) {
	f, err := c.Read("linear", env)
	if err != nil {
		return nil, deny(503, "linear: %s", err.Error())
	}
	return f, nil
}

func fresh(f map[string]string, now time.Time) bool {
	if f["access_token"] == "" {
		return false
	}
	raw, has := f["expires_at"]
	if !has {
		return true
	}
	exp, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return false
	}
	return time.Unix(int64(exp), 0).Add(-refreshMargin).After(now)
}

// AccessToken is a usable access token for one env, refreshed when it is
// stale or when the caller just watched Linear reject `rejected`. It
// re-reads the store under the lock first: someone else may have refreshed
// already, and their token is the answer.
func (o oauth) accessToken(ctx context.Context, c *creds.Store, env, rejected string) (string, error) {
	f, err := readLogin(c, env)
	if err != nil {
		return "", err
	}
	if t := f["access_token"]; t != "" && t != rejected && fresh(f, o.now()) {
		return t, nil
	}
	l := refreshLock(c, env)
	l.Lock()
	defer l.Unlock()
	if f, err = readLogin(c, env); err != nil {
		return "", err
	}
	if t := f["access_token"]; t != "" && t != rejected && fresh(f, o.now()) {
		return t, nil
	}
	answer, err := o.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {f["refresh_token"]},
		"client_id":     {f["client_id"]},
		"resource":      {o.resource()},
	}, env)
	if err != nil {
		return "", err
	}
	if err := o.store(c, env, f["client_id"], answer, f["refresh_token"]); err != nil {
		return "", err
	}
	return answer["access_token"].(string), nil
}

func (o oauth) store(c *creds.Store, env, clientID string, answer map[string]any, previousRefresh string) error {
	values := map[string]string{"client_id": clientID, "access_token": answer["access_token"].(string)}
	// Rotation is the server's call; without a new one the old stands.
	values["refresh_token"] = previousRefresh
	if r, _ := answer["refresh_token"].(string); r != "" {
		values["refresh_token"] = r
	}
	if exp := number(answer["expires_in"]); exp > 0 {
		values["expires_at"] = strconv.FormatInt(o.now().Unix()+int64(exp), 10)
	}
	return c.Write("linear", env, values)
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

// Pending is a login in progress: what came back must match it.
type Pending struct {
	ClientID, Verifier, State, Redirect string
	URL                                 string
	Expires                             time.Time
}

func randomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// PKCEPair is a verifier and its S256 challenge.
func PKCEPair() (verifier, challenge string) {
	verifier = randomString(64)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// startLogin registers a public client for this one redirect (one per
// login, so a lost one costs nothing) and returns the URL to approve.
func (o oauth) startLogin(ctx context.Context, env, redirect string) (Pending, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "gero (" + env + ")",
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      Scope,
	})
	status, answer, err := o.post(ctx, "/register", "application/json", body)
	if err != nil {
		return Pending{}, deny(502, "linear: client registration is unreachable: %v", err)
	}
	clientID, _ := answer["client_id"].(string)
	if (status != 200 && status != 201) || clientID == "" {
		return Pending{}, deny(502, "linear: client registration answered HTTP %d (%v)", status, answer)
	}
	verifier, challenge := PKCEPair()
	p := Pending{ClientID: clientID, Verifier: verifier, State: randomString(24), Redirect: redirect,
		Expires: o.now().Add(15 * time.Minute)}
	p.URL = o.issuer + "/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {p.State},
		"scope":                 {Scope},
		"resource":              {o.resource()},
	}.Encode()
	return p, nil
}

// callbackQuery takes what the user pasted (the whole address the browser
// landed on, or just its query) and returns its parameters.
func callbackQuery(pasted string) url.Values {
	pasted = strings.TrimSpace(pasted)
	if i := strings.IndexByte(pasted, '?'); i >= 0 {
		pasted = pasted[i+1:]
	}
	if i := strings.IndexByte(pasted, '#'); i >= 0 {
		pasted = pasted[:i]
	}
	q, _ := url.ParseQuery(pasted)
	return q
}

// finishLogin checks what came back against the login it belongs to,
// exchanges the code, and stores the tokens. The state and the PKCE
// verifier make a code that arrives any other way worthless.
func (o oauth) finishLogin(ctx context.Context, c *creds.Store, env string, p Pending, pasted string) error {
	if o.now().After(p.Expires) {
		return deny(400, "linear: this login was started more than 15 minutes ago; start it again")
	}
	q := callbackQuery(pasted)
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(p.State)) != 1 {
		return deny(400, "linear: the login came back with the wrong state; it was not the one this login started")
	}
	if iss := q.Get("iss"); iss != "" && iss != o.issuer {
		return deny(400, "linear: the login came back from issuer %q, not %s", iss, o.issuer)
	}
	code := q.Get("code")
	if code == "" {
		why := q.Get("error_description")
		if why == "" {
			why = q.Get("error")
		}
		if why == "" {
			why = "no code"
		}
		return deny(400, "linear: the login was refused: %s", why)
	}
	tokens, err := o.tokenRequest(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.Redirect},
		"client_id":     {p.ClientID},
		"code_verifier": {p.Verifier},
		"resource":      {o.resource()},
	}, env)
	if err != nil {
		return err
	}
	if r, _ := tokens["refresh_token"].(string); r == "" {
		return deny(502, "linear: the login answered without a refresh token; the proxy would lose access within the hour")
	}
	l := refreshLock(c, env)
	l.Lock()
	defer l.Unlock()
	return o.store(c, env, p.ClientID, tokens, "")
}
