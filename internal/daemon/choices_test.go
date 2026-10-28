package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/gitproxy"
)

func TestChoicesAreLookedUpWithTheTypedOrStoredCredential(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if who != "stored" && who != "typed" {
			http.Error(w, "{}", 401)
			return
		}
		fmt.Fprintf(w, `[{"full_name":"%s/repo","permissions":{"push":true}}]`, who)
	}))
	defer gh.Close()
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	c := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	m.SetProxies(proxy.NewRegistry(gitproxy.Service{APIUpstream: gh.URL}), c)
	ctx := context.Background()

	v, err := m.PutEnv(ctx, "git", "work", proxy.Patch{Set: map[string]any{"repos": []any{"stored/repo"}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.MissingSecrets) != 1 || v.CredentialElsewhere {
		t.Fatalf("an env with no credential: %+v", v)
	}
	value := func(req api.ChoicesRequest) string {
		t.Helper()
		cs, err := m.Choices(ctx, "git", "repos", req)
		if err != nil {
			return err.Error()
		}
		var out []string
		for _, c := range cs {
			out = append(out, c.Value)
		}
		return strings.Join(out, ",")
	}
	if got := value(api.ChoicesRequest{}); !strings.Contains(got, "enter a token first") {
		t.Fatalf("no credential: %s", got)
	}
	if _, err := m.Choices(ctx, "git", "repos", api.ChoicesRequest{}); err == nil {
		t.Fatal("no error")
	} else if he, ok := err.(httpError); !ok || he.code != 400 {
		t.Fatalf("not a 400: %v", err)
	}
	if got := value(api.ChoicesRequest{Secret: map[string]string{"token": " typed "}}); got != "typed/repo" {
		t.Fatalf("typed: %s", got)
	}
	if err := m.SetSecret("git", "work", map[string]string{"token": "stored"}); err != nil {
		t.Fatal(err)
	}
	if got := value(api.ChoicesRequest{Env: "work"}); got != "stored/repo" {
		t.Fatalf("stored: %s", got)
	}
	if got := value(api.ChoicesRequest{Env: "work", Secret: map[string]string{"token": "typed"}}); got != "typed/repo" {
		t.Fatalf("a typed token does not win over the stored one: %s", got)
	}
	if _, err := m.Choices(ctx, "git", "read_only", api.ChoicesRequest{}); err == nil {
		t.Fatal("a field with no lookup answered")
	}

	v, err = m.PutEnv(ctx, "git", "gh", proxy.Patch{Set: map[string]any{"token_command": "sh"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !v.CredentialElsewhere || len(v.MissingSecrets) != 0 {
		t.Fatalf("an env with a token command: %+v", v)
	}
}
