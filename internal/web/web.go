// Package web serves gero's browser UI and puts the daemon API behind a
// token, for the daemon's TCP listener. The listener is loopback-only;
// reaching it from a phone is `tailscale serve`'s job, which brings HTTPS and
// the tailnet's own access control. The token is what stands between the
// API and anything else that can reach the port.
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed static
var static embed.FS

const (
	cookieName = "gero_session"
	// csrfHeader must accompany every request that changes something. A
	// cross-site page cannot set a custom header without a CORS preflight,
	// which this server never approves.
	csrfHeader = "X-Gero-Request"
)

// Server is the browser-facing handler. TokenFile is read again when it
// changes, so rotating the token takes effect without a restart.
type Server struct {
	API       http.Handler
	TokenFile string
	// Hosts are extra Host header names to accept besides loopback names and
	// *.ts.net (what `tailscale serve` forwards).
	Hosts []string

	mu      sync.Mutex
	token   string
	modTime time.Time
}

// EnsureToken creates the token file unless it exists and returns the token.
func EnsureToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	return RotateToken(path)
}

// RotateToken writes a new token, which signs every browser out.
func RotateToken(path string) (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, os.Rename(tmp, path)
}

func (s *Server) currentToken() string {
	st, err := os.Stat(s.TokenFile)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !st.ModTime().Equal(s.modTime) {
		b, err := os.ReadFile(s.TokenFile)
		if err != nil {
			return ""
		}
		s.token, s.modTime = strings.TrimSpace(string(b)), st.ModTime()
	}
	return s.token
}

func (s *Server) valid(candidate string) bool {
	tok := s.currentToken()
	return tok != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(tok)) == 1
}

func (s *Server) Handler() http.Handler {
	assets, _ := fs.Sub(static, "static")
	files := http.FileServer(http.FS(assets))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "index.html")
	})
	mux.Handle("GET /assets/", files)
	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("POST /v1/session", s.session)
	mux.HandleFunc("DELETE /v1/session", s.logout)
	mux.Handle("/v1/", s.requireAuth(s.API))
	return s.checkHost(secure(mux))
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Agent output is untrusted text; the UI never renders it as HTML,
		// and this policy would refuse an injected script if it did.
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// checkHost refuses Host names the server was never meant to answer, which
// takes DNS rebinding off the table even before the token check.
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(strings.ToLower(host), ".")
		ok := host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".ts.net")
		for _, h := range s.Hosts {
			ok = ok || host == strings.ToLower(h)
		}
		if !ok {
			http.Error(w, "unexpected Host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) (ok, bearer bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return s.valid(strings.TrimPrefix(h, "Bearer ")), true
	}
	c, err := r.Cookie(cookieName)
	return err == nil && s.valid(c.Value), false
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, bearer := s.authorized(r)
		if !ok {
			jsonError(w, http.StatusUnauthorized, "sign in with the link from `gero web`")
			return
		}
		// A bearer token is never sent by a browser on its own, so only
		// cookie sessions need the CSRF header.
		if !bearer && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != "1" {
			jsonError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}

// login exchanges the token in a link for a session cookie and takes the
// token out of the address bar.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.valid(r.URL.Query().Get("token")) {
		http.Error(w, "this sign-in link is not valid any more; run `gero web` for a new one", http.StatusUnauthorized)
		return
	}
	s.setCookie(w, r, s.currentToken(), 30*24*3600)
	http.Redirect(w, r, localPath(r.URL.Query().Get("next")), http.StatusSeeOther)
}

// localPath keeps a post-login redirect on this server: a path, never a
// scheme-relative or absolute URL that would hand the visitor elsewhere.
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") || strings.ContainsAny(next, "\r\n") {
		return "/"
	}
	return next
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if !s.valid(strings.TrimSpace(req.Token)) {
		jsonError(w, http.StatusUnauthorized, "that token is not valid")
		return
	}
	s.setCookie(w, r, s.currentToken(), 30*24*3600)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.setCookie(w, r, "", -1)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
