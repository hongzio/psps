package creds

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ported from isolate's tests/test_credstore.py. One file holds every
// proxy secret, so its failure modes must carry the exact edit that fixes
// them: which section, which fields, which chmod.

func newStore(t *testing.T) *Store {
	return &Store{Path: filepath.Join(t.TempDir(), "credentials")}
}

func reason(t *testing.T, err error) string {
	t.Helper()
	var d *Denied
	if !errors.As(err, &d) {
		t.Fatalf("want a Denied, got %v", err)
	}
	return d.Reason
}

func TestRoundTripAnd0600(t *testing.T) {
	s := newStore(t)
	if err := s.Write("datadog", "prod", map[string]string{"api_key": "AAA", "app_key": "BBB"}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(s.Path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
	got, err := s.Read("datadog", "prod")
	if err != nil || got["api_key"] != "AAA" || got["app_key"] != "BBB" || len(got) != 2 {
		t.Fatalf("read = %v, %v", got, err)
	}
}

func TestWritingOneSectionKeepsTheOthers(t *testing.T) {
	s := newStore(t)
	s.Write("slack", "dev", map[string]string{"token": "xoxb-1"})
	s.Write("pg", "prod", map[string]string{"password": "pw"})
	if got, _ := s.Read("slack", "dev"); got["token"] != "xoxb-1" {
		t.Fatalf("slack dev = %v", got)
	}
}

func TestRewritingASectionReplacesItsFields(t *testing.T) {
	s := newStore(t)
	s.Write("slack", "dev", map[string]string{"token": "old"})
	s.Write("slack", "dev", map[string]string{"token": "new"})
	if got, _ := s.Read("slack", "dev"); got["token"] != "new" {
		t.Fatalf("token = %v", got)
	}
}

func TestAMissingFileNamesPathSectionAndFields(t *testing.T) {
	s := newStore(t)
	_, err := s.Read("pg", "prod")
	r := reason(t, err)
	for _, needle := range []string{s.Path, "[pg prod]", "password", "gero env secret pg prod"} {
		if !strings.Contains(r, needle) {
			t.Errorf("refusal lacks %q: %s", needle, r)
		}
	}
}

func TestAMissingSectionIsNamed(t *testing.T) {
	s := newStore(t)
	s.Write("pg", "prod", map[string]string{"password": "x"})
	_, err := s.Read("datadog", "prod")
	if r := reason(t, err); !strings.Contains(r, "[datadog prod]") {
		t.Fatal(r)
	}
}

func TestAMissingFieldIsNamed(t *testing.T) {
	s := newStore(t)
	s.Write("datadog", "prod", map[string]string{"api_key": "AAA"})
	_, err := s.Read("datadog", "prod")
	if r := reason(t, err); !strings.Contains(r, "app_key") {
		t.Fatal(r)
	}
	if missing, _ := s.Status("datadog", "prod"); len(missing) != 1 || missing[0] != "app_key" {
		t.Fatalf("status = %v", missing)
	}
}

func TestGroupReadableIsRefusedWithTheChmod(t *testing.T) {
	s := newStore(t)
	s.Write("pg", "prod", map[string]string{"password": "x"})
	os.Chmod(s.Path, 0o640)
	_, err := s.Read("pg", "prod")
	if r := reason(t, err); !strings.Contains(r, "chmod 600") {
		t.Fatal(r)
	}
	if err := s.Write("pg", "prod", map[string]string{"password": "y"}); err == nil {
		t.Fatal("a write repaired a loose file instead of refusing")
	}
}

func TestAPercentSignSurvivesTheRoundTrip(t *testing.T) {
	// Passwords are opaque; configparser's default interpolation would read
	// a lone % as syntax.
	s := newStore(t)
	s.Write("pg", "prod", map[string]string{"password": "p%ss%%word"})
	if got, _ := s.Read("pg", "prod"); got["password"] != "p%ss%%word" {
		t.Fatalf("password = %q", got["password"])
	}
}

func TestAnIsolateFileReadsAsIs(t *testing.T) {
	// What Python's configparser writes, comments and a colon delimiter
	// included, so an isolate credentials file can be brought over.
	s := newStore(t)
	os.WriteFile(s.Path, []byte("; written by isolate\n[pg prod]\npassword = pw=with=equals\n\n[datadog prod]\nAPI_KEY: aaa\napp_key = bbb\n"), 0o600)
	if got, err := s.Read("pg", "prod"); err != nil || got["password"] != "pw=with=equals" {
		t.Fatalf("pg = %v %v", got, err)
	}
	if got, err := s.Read("datadog", "prod"); err != nil || got["api_key"] != "aaa" {
		t.Fatalf("datadog = %v %v", got, err)
	}
}

func TestOptionalFieldsAreKeptAndRemovingASectionWorks(t *testing.T) {
	s := newStore(t)
	s.Write("notion", "main", map[string]string{"client_id": "c", "refresh_token": "r", "access_token": "a"})
	if got, _ := s.Read("notion", "main"); got["access_token"] != "a" {
		t.Fatalf("optional field dropped: %v", got)
	}
	s.Write("notion", "main", nil)
	if _, err := s.Read("notion", "main"); !strings.Contains(reason(t, err), "browser") {
		t.Fatal("a login service's refusal should name the login")
	}
}

func TestAValueTheFileCannotHoldIsRefused(t *testing.T) {
	s := newStore(t)
	if err := s.Write("slack", "dev", map[string]string{"token": "a\nb"}); err == nil {
		t.Fatal("a multi-line value was written")
	}
}
