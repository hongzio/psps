package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newServer(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "web-token")
	tok, err := EnsureToken(path)
	if err != nil {
		t.Fatal(err)
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("api " + r.Method)) })
	return &Server{API: api, TokenFile: path}, tok
}

func do(h http.Handler, method, url string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, nil)
	r.Host = "127.0.0.1:4376"
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAPIRequiresTheToken(t *testing.T) {
	s, tok := newServer(t)
	h := s.Handler()
	if w := do(h, "GET", "/v1/agents", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: %d", w.Code)
	}
	if w := do(h, "GET", "/v1/agents", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: %d", w.Code)
	}
	if w := do(h, "POST", "/v1/agents", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }); w.Body.String() != "api POST" {
		t.Fatalf("bearer: %d %s", w.Code, w.Body)
	}
}

func TestLoginLinkSetsAStrictCookie(t *testing.T) {
	s, tok := newServer(t)
	h := s.Handler()
	w := do(h, "GET", "/login?token="+tok, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("login: %d %v", w.Code, w.Header())
	}
	c := w.Result().Cookies()[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Value != tok {
		t.Fatalf("cookie = %+v", c)
	}
	withCookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: cookieName, Value: tok}) }
	if w := do(h, "GET", "/v1/agents", withCookie); w.Body.String() != "api GET" {
		t.Fatalf("cookie GET: %d", w.Code)
	}
	// A cookie alone cannot change anything: the CSRF header is required.
	if w := do(h, "POST", "/v1/agents", withCookie); w.Code != http.StatusForbidden {
		t.Fatalf("cookie POST without header: %d", w.Code)
	}
	if w := do(h, "POST", "/v1/agents", func(r *http.Request) { withCookie(r); r.Header.Set(csrfHeader, "1") }); w.Body.String() != "api POST" {
		t.Fatalf("cookie POST with header: %d", w.Code)
	}
	if w := do(h, "GET", "/login?token=bad", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad login link: %d", w.Code)
	}
}

func TestRotatingTheTokenSignsEveryoneOut(t *testing.T) {
	s, tok := newServer(t)
	h := s.Handler()
	time.Sleep(10 * time.Millisecond) // a distinct mtime
	if _, err := RotateToken(s.TokenFile); err != nil {
		t.Fatal(err)
	}
	if w := do(h, "GET", "/v1/agents", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }); w.Code != http.StatusUnauthorized {
		t.Fatalf("old token still works: %d", w.Code)
	}
}

func TestHostCheckAndHeaders(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	if w := do(h, "GET", "/", func(r *http.Request) { r.Host = "evil.example" }); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host: %d", w.Code)
	}
	w := do(h, "GET", "/", func(r *http.Request) { r.Host = "mini.example.ts.net" })
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<title>gero</title>") {
		t.Fatalf("index via ts.net: %d", w.Code)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}
	if w := do(h, "GET", "/assets/app.js", nil); w.Code != http.StatusOK {
		t.Fatalf("app.js: %d", w.Code)
	}
}

func TestLoginRedirectStaysLocal(t *testing.T) {
	s, tok := newServer(t)
	h := s.Handler()
	for next, want := range map[string]string{
		"/#/isolations":        "/#/isolations",
		"//evil.example/":      "/",
		"https://evil.example": "/",
		"/\\evil.example":      "/",
		"":                     "/",
	} {
		w := do(h, "GET", "/login?token="+tok+"&next="+url.QueryEscape(next), nil)
		if got := w.Header().Get("Location"); got != want {
			t.Errorf("next=%q redirected to %q, want %q", next, got, want)
		}
	}
}
