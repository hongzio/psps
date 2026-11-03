package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/paths"
	"github.com/hongzio/gero/internal/store"
)

func home(t *testing.T) paths.Paths {
	d := t.TempDir()
	p := paths.Paths{Config: filepath.Join(d, "config"), Data: filepath.Join(d, "data"), State: filepath.Join(d, "state")}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	return p
}

func write(t *testing.T, file, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestABackupIsRestoredElsewhereWithoutWhatADaemonMakesAgain(t *testing.T) {
	src := home(t)
	st, err := store.Open(src.DB())
	if err != nil {
		t.Fatal(err)
	}
	// Left open, as a running daemon leaves it: the row is in the WAL.
	defer st.Close()
	if err := st.PutIsolation(store.Isolation{ID: "7f0b9b73", Name: "dev", Container: "gero-7f0b9b73", Image: "img", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	write(t, src.Credentials(), "[git work]\ntoken = x\n", 0o600)
	write(t, filepath.Join(src.Skills(), "ship", "SKILL.md"), "ship it", 0o644)
	write(t, src.WebToken(), "tok", 0o600)
	write(t, src.Credentials()+".lock", "", 0o600)
	write(t, src.Lock(), "123\n", 0o600)
	write(t, src.Log(), "log", 0o644)
	t.Chdir(src.State) // a socket's path is short
	sock, err := net.Listen("unix", "s.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()

	file := filepath.Join(t.TempDir(), "b.tar.gz")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Write(f, src)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 4 || len(sum.Skipped) != 0 {
		t.Errorf("summary %+v", sum)
	}

	dst := home(t)
	if _, err := Restore(file, dst, false); err != nil {
		t.Fatal(err)
	}
	got, err := store.Open(dst.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if iso, err := got.Isolation("7f0b9b73"); err != nil || iso.Container != "gero-7f0b9b73" {
		t.Errorf("isolation %+v %v", iso, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst.Skills(), "ship", "SKILL.md")); string(b) != "ship it" {
		t.Errorf("skill %q", b)
	}
	if fi, err := os.Stat(dst.Credentials()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials %v %v", fi, err)
	}
	for _, f := range []string{dst.Credentials() + ".lock", dst.Log()} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was restored", f)
		}
	}

	// Over a store it goes only with force.
	if _, err := Restore(file, dst, false); !errors.Is(err, ErrExists) {
		t.Errorf("over a store: %v", err)
	}
	if _, err := Restore(file, dst, true); err != nil {
		t.Errorf("forced: %v", err)
	}
}

func TestARestoreWaitsForNoDaemonAndChangesNothingFromABadArchive(t *testing.T) {
	p := home(t)
	lock, err := os.OpenFile(p.Lock(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	f, _ := os.Create(bad)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"store.db", "config/../../escape"} {
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: 1})
		tw.Write([]byte("x"))
	}
	tw.Close()
	gz.Close()
	f.Close()

	if _, err := Restore(bad, p, false); !errors.Is(err, ErrRunning) {
		t.Errorf("with a daemon: %v", err)
	}
	lock.Close()
	if _, err := Restore(bad, p, false); err == nil {
		t.Error("an entry outside was restored")
	}
	if _, err := os.Stat(p.DB()); err == nil {
		t.Error("the store was written from a bad archive")
	}
}
