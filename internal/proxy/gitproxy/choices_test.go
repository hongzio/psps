package gitproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
)

// fakeUserRepos is GitHub for the token "good": /user, and /user/repos in
// two pages, the first naming the second in its Link header, as GitHub does.
func fakeUserRepos(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, `{"message":"Bad credentials"}`, 401)
			return
		}
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"login":"me"}`)
			return
		case "/user/repos":
		default:
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"full_name":"acme/Old","archived":true,"permissions":{"push":true}}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/user/repos?page=2>; rel="next", <%s/user/repos?page=2>; rel="last"`, srv.URL, srv.URL))
		fmt.Fprint(w, `[{"full_name":"me/site","private":false,"permissions":{"push":true}},
			{"full_name":"acme/app","private":true,"permissions":{"push":false}}]`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTheRepositoriesATokenOpensAreTheChoices(t *testing.T) {
	s := Service{APIUpstream: fakeUserRepos(t).URL}
	cs, err := s.Choices(context.Background(), "repos", map[string]any{}, map[string]string{"token": "good"})
	if err != nil {
		t.Fatal(err)
	}
	want := []proxy.Choice{{Value: "acme/app", Note: "private, read-only"}, {Value: "acme/Old", Note: "archived"}, {Value: "me/site"}}
	if fmt.Sprint(cs) != fmt.Sprint(want) {
		t.Fatalf("%v", cs)
	}

	var d *proxy.Denied
	if _, err := s.Choices(context.Background(), "repos", map[string]any{}, nil); !errors.As(err, &d) || !strings.Contains(err.Error(), "enter a token first") {
		t.Fatalf("no credential: %v", err)
	}
	if _, err := s.Choices(context.Background(), "repos", map[string]any{}, map[string]string{"token": "bad"}); !errors.As(err, &d) || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a refused token: %v", err)
	}
	// Without a token, the token command's, as typed in a form or as stored.
	for _, tc := range []any{[]any{"/bin/echo", "good"}, []string{"/bin/echo", "good"}} {
		if cs, err := s.Choices(context.Background(), "repos", map[string]any{"token_command": tc}, nil); err != nil || len(cs) != 3 {
			t.Fatalf("%v: %v %v", tc, cs, err)
		}
	}
	if _, err := s.Choices(context.Background(), "read_only", nil, nil); err == nil {
		t.Fatal("a field with no lookup answered")
	}
}

func TestAStoredTokenWinsOverTheTokenCommand(t *testing.T) {
	srv := fakeUserRepos(t)
	c := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	s := Service{APIUpstream: srv.URL}
	cfg := json.RawMessage(`{"repos":["me/site"],"token_command":["/bin/echo","bad"]}`)
	if !s.CredentialElsewhere(cfg) || s.CredentialElsewhere(json.RawMessage(`{"repos":[]}`)) {
		t.Fatal("CredentialElsewhere does not follow the token command")
	}
	if _, err := s.Check(context.Background(), "work", cfg, c); err == nil {
		t.Fatal("the command's refused token checked out")
	}
	if err := c.Write("git", "work", map[string]string{"token": "good"}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Check(context.Background(), "work", cfg, c)
	if err != nil || !strings.Contains(out, "credential stored token") || !strings.Contains(out, "authenticates as me") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := s.Check(context.Background(), "solo", json.RawMessage(`{"repos":[]}`), c); err == nil ||
		!strings.Contains(err.Error(), "gero env secret git solo") {
		t.Fatalf("no credential at all: %v", err)
	}
}

func TestAnEnvWithoutATokenCommandIsNotGivenOne(t *testing.T) {
	env := map[string]any{"repos": []any{"me/site"}}
	if _, err := (Service{}).Normalize("work", env); err != nil {
		t.Fatal(err)
	}
	if _, has := env["token_command"]; has {
		t.Fatalf("given %v", env["token_command"])
	}
	env = map[string]any{"token_command": "sh"}
	if _, err := (Service{}).Normalize("work", env); err != nil {
		t.Fatal(err)
	}
	if cmd := env["token_command"].([]string); !strings.HasSuffix(cmd[0], "/sh") || cmd[1] != "auth" {
		t.Fatalf("pinned %v", cmd)
	}
}
