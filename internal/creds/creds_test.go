package creds

import (
	"encoding/base64"
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
	dir := t.TempDir()
	return &Store{Path: filepath.Join(dir, "config", "credentials"), KeyPath: filepath.Join(dir, "state", "credentials.key")}
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
	for _, needle := range []string{s.Path, "[pg prod]", "password", "psps env secret pg prod"} {
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

func TestAHandWrittenFileReadsAsIs(t *testing.T) {
	// Plain values written by hand, comments and a colon delimiter
	// included, are read without any key.
	s := newStore(t)
	os.MkdirAll(filepath.Dir(s.Path), 0o700)
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

func TestValuesAreWrittenEncryptedWithTheKeyElsewhere(t *testing.T) {
	s := newStore(t)
	s.Write("pg", "prod", map[string]string{"password": "hunter2"})
	b, _ := os.ReadFile(s.Path)
	if strings.Contains(string(b), "hunter2") || !strings.Contains(string(b), "[pg prod]") || !strings.Contains(string(b), "password = enc:v1:") {
		t.Fatalf("file = %s", b)
	}
	st, err := os.Stat(s.KeyPath)
	if err != nil || st.Mode().Perm() != 0o600 || st.Size() != 32 {
		t.Fatalf("key: %v %v", st, err)
	}
	if plain, _ := s.Plain(); len(plain) != 0 {
		t.Fatalf("plain = %v", plain)
	}
}

func TestPlainValuesAreNamedAndEncryptSealsThem(t *testing.T) {
	s := newStore(t)
	os.MkdirAll(filepath.Dir(s.Path), 0o700)
	os.WriteFile(s.Path, []byte("[pg prod]\npassword = pw\n\n[slack dev]\ntoken = xoxb\n"), 0o600)
	if plain, _ := s.Plain(); len(plain) != 2 {
		t.Fatalf("plain = %v", plain)
	}
	if n, err := s.Encrypt(); err != nil || n != 2 {
		t.Fatalf("encrypt = %d, %v", n, err)
	}
	b, _ := os.ReadFile(s.Path)
	if strings.Contains(string(b), "xoxb") {
		t.Fatalf("file = %s", b)
	}
	if got, err := s.Read("slack", "dev"); err != nil || got["token"] != "xoxb" {
		t.Fatalf("slack = %v %v", got, err)
	}
	if n, err := s.Encrypt(); err != nil || n != 0 {
		t.Fatalf("second encrypt = %d, %v", n, err)
	}
}

func TestAnyWriteSealsAHandAddedValue(t *testing.T) {
	s := newStore(t)
	s.Write("pg", "prod", map[string]string{"password": "pw"})
	f, _ := os.OpenFile(s.Path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[slack dev]\ntoken = xoxb\n")
	f.Close()
	s.Write("pg", "dev", map[string]string{"password": "pw2"})
	if plain, _ := s.Plain(); len(plain) != 0 {
		t.Fatalf("plain = %v", plain)
	}
	if got, _ := s.Read("slack", "dev"); got["token"] != "xoxb" {
		t.Fatalf("slack = %v", got)
	}
}

func TestEncryptWithoutAFileMakesNothing(t *testing.T) {
	s := newStore(t)
	if n, err := s.Encrypt(); err != nil || n != 0 {
		t.Fatalf("encrypt = %d, %v", n, err)
	}
	if _, err := os.Stat(s.KeyPath); err == nil {
		t.Fatal("a key was made with nothing to seal")
	}
}

func TestAMissingKeyIsNamed(t *testing.T) {
	s := newStore(t)
	s.Write("pg", "prod", map[string]string{"password": "pw"})
	os.Remove(s.KeyPath)
	_, err := s.Read("pg", "prod")
	if r := reason(t, err); !strings.Contains(r, s.KeyPath) || !strings.Contains(r, "missing") {
		t.Fatal(r)
	}
	if err := s.Write("pg", "dev", map[string]string{"password": "x"}); err == nil {
		t.Fatal("a write went ahead without the key the file was sealed with")
	}
}

func TestAWrongKeyOrAMovedValueDoesNotOpen(t *testing.T) {
	s := newStore(t)
	s.Write("pg", "prod", map[string]string{"password": "pw"})
	s.Write("pg", "dev", map[string]string{"password": "other"})
	b, _ := os.ReadFile(s.Path)
	// Swap the two sealed values: each is bound to its own section.
	lines := strings.Split(string(b), "\n")
	var idx []int
	for i, l := range lines {
		if strings.HasPrefix(l, "password = ") {
			idx = append(idx, i)
		}
	}
	lines[idx[0]], lines[idx[1]] = lines[idx[1]], lines[idx[0]]
	os.WriteFile(s.Path, []byte(strings.Join(lines, "\n")), 0o600)
	if _, err := s.Read("pg", "prod"); err == nil {
		t.Fatal("a value moved to another section opened")
	}

	s2 := newStore(t)
	s2.Write("pg", "prod", map[string]string{"password": "pw"})
	os.WriteFile(s2.KeyPath, make([]byte, 32), 0o600)
	if _, err := s2.Read("pg", "prod"); err == nil {
		t.Fatal("a value opened with another key")
	}
}

func TestALooseKeyIsRefused(t *testing.T) {
	s := newStore(t)
	s.Write("pg", "prod", map[string]string{"password": "pw"})
	os.Chmod(s.KeyPath, 0o644)
	if r := reason(t, func() error { _, err := s.Read("pg", "prod"); return err }()); !strings.Contains(r, "chmod 600 "+s.KeyPath) {
		t.Fatal(r)
	}
}

func TestABase64KeyFromTheFirstBuildStillOpens(t *testing.T) {
	s := newStore(t)
	s.Write("pg", "prod", map[string]string{"password": "pw"})
	raw, _ := os.ReadFile(s.KeyPath)
	os.WriteFile(s.KeyPath, []byte(base64.StdEncoding.EncodeToString(raw)+"\n"), 0o600)
	if got, err := s.Read("pg", "prod"); err != nil || got["password"] != "pw" {
		t.Fatalf("read = %v %v", got, err)
	}
}

func TestAKeyOfTheWrongSizeIsRefused(t *testing.T) {
	s := newStore(t)
	s.Write("pg", "prod", map[string]string{"password": "pw"})
	os.WriteFile(s.KeyPath, []byte("short"), 0o600)
	if r := reason(t, func() error { _, err := s.Read("pg", "prod"); return err }()); !strings.Contains(r, "not a credentials key") {
		t.Fatal(r)
	}
}

func TestRenameMovesTheSectionAndRefusesATakenName(t *testing.T) {
	s := newStore(t)
	if err := s.Rename("git", "work", "job"); err != nil {
		t.Fatalf("renaming with no file: %v", err)
	}
	s.Write("datadog", "prod", map[string]string{"api_key": "AAA", "app_key": "BBB"})
	s.Write("datadog", "stage", map[string]string{"api_key": "CCC", "app_key": "DDD"})
	if err := s.Rename("datadog", "prod", "live"); err != nil {
		t.Fatal(err)
	}
	// Sealed under the section's name, it opens under the new one.
	got, err := s.Read("datadog", "live")
	if err != nil || got["api_key"] != "AAA" || got["app_key"] != "BBB" {
		t.Fatalf("live = %v %v", got, err)
	}
	if _, err := s.Read("datadog", "prod"); err == nil {
		t.Fatal("prod is still there")
	}
	b, _ := os.ReadFile(s.Path)
	if i, j := strings.Index(string(b), "[datadog live]"), strings.Index(string(b), "[datadog stage]"); i < 0 || j < i {
		t.Fatalf("the section moved from where it stood:\n%s", b)
	}
	if r := reason(t, s.Rename("datadog", "live", "stage")); !strings.Contains(r, "[datadog stage]") || !strings.Contains(r, "--clear") {
		t.Fatalf("reason = %s", r)
	}
	if got, _ := s.Read("datadog", "stage"); got["api_key"] != "CCC" {
		t.Fatalf("a refused rename changed stage: %v", got)
	}
	if err := s.Rename("datadog", "nothing", "else"); err != nil {
		t.Fatalf("renaming a section that is not there: %v", err)
	}
}
