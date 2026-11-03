// Package backup saves what psps keeps on the Mac to one file and puts it
// back: the store, the settings, the credentials and skills, and the state
// worth keeping (the web UI's token, a bastion's known hosts). It leaves out
// what a daemon makes again (its socket, locks and log), and the
// isolations' volumes, which live with container, not with psps.
//
// The archive names nothing after where it came from: the store is
// store.db and everything else is under config/, data/ or state/, relative
// to those directories. A psps at other paths, or under another name,
// restores it into its own.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hongzio/psps/internal/paths"
	"github.com/hongzio/psps/internal/store"
)

// storeName is the store's name in an archive.
const storeName = "store.db"

// Summary is what a backup holds, or a restore put back.
type Summary struct {
	Files int
	Bytes int64
	// Skipped are what a backup left out that it could not read as a
	// plain file, relative to its archive.
	Skipped []string
}

// roots are the directories an archive holds, by their name in it.
func roots(p paths.Paths) [][2]string {
	return [][2]string{{"config", p.Config}, {"data", p.Data}, {"state", p.State}}
}

// kept says whether a file a backup comes across belongs in it.
func kept(p paths.Paths, file string) bool {
	base := filepath.Base(file)
	if strings.HasSuffix(base, ".lock") || strings.HasPrefix(base, filepath.Base(p.Log())) {
		return false
	}
	db := p.DB()
	return file != db && file != db+"-wal" && file != db+"-shm" && file != db+"-journal"
}

// Write writes the archive of p to w. The store is snapshotted, so a daemon
// may keep running meanwhile.
func Write(w io.Writer, p paths.Paths) (Summary, error) {
	var sum Summary
	tmp, err := os.MkdirTemp("", "psps-backup")
	if err != nil {
		return sum, err
	}
	defer os.RemoveAll(tmp)
	snap := filepath.Join(tmp, storeName)
	if err := store.Snapshot(p.DB(), snap); err != nil {
		return sum, err
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := addFile(tw, snap, storeName, &sum); err != nil {
		return sum, err
	}
	for _, r := range roots(p) {
		err := filepath.WalkDir(r[1], func(file string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && file == r[1] {
					return filepath.SkipDir
				}
				return err
			}
			rel, _ := filepath.Rel(r[1], file)
			name := path.Join(r[0], filepath.ToSlash(rel))
			switch {
			case d.IsDir():
				return nil
			case !d.Type().IsRegular():
				if d.Type()&fs.ModeSocket == 0 {
					sum.Skipped = append(sum.Skipped, name)
				}
				return nil
			case !kept(p, file):
				return nil
			}
			return addFile(tw, file, name, &sum)
		})
		if err != nil {
			return sum, err
		}
	}
	if err := tw.Close(); err != nil {
		return sum, err
	}
	return sum, gz.Close()
}

func addFile(tw *tar.Writer, file, name string, sum *Summary) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	h := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: fi.Size(), Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime()}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	n, err := io.Copy(tw, f)
	if err != nil {
		return err
	}
	if n != fi.Size() {
		return fmt.Errorf("%s changed while it was saved", file)
	}
	sum.Files++
	sum.Bytes += n
	return nil
}

// ErrRunning is a restore refused because a daemon holds p's lock.
var ErrRunning = errors.New("a psps daemon is running; stop it first (psps daemon stop)")

// ErrExists is a restore refused because p already has a store.
var ErrExists = errors.New("there is a store here already; restoring over it replaces it")

// Restore puts the archive at file back into p. No daemon may be running:
// Restore holds its lock while it works, so none starts meanwhile. Over a
// store that exists it goes only with force, and then replaces the files
// the archive has, leaving others as they are.
func Restore(file string, p paths.Paths, force bool) (Summary, error) {
	var sum Summary
	if err := p.Ensure(); err != nil {
		return sum, err
	}
	lock, err := os.OpenFile(p.Lock(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return sum, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return sum, ErrRunning
	}
	if _, err := os.Stat(p.DB()); err == nil && !force {
		return sum, ErrExists
	}

	// Read it through once before anything is written, so a bad archive
	// changes nothing.
	dests := map[string]string{}
	err = entries(file, func(h *tar.Header, _ io.Reader) error {
		dst, err := destination(p, h.Name)
		if err != nil {
			return err
		}
		dests[h.Name] = dst
		return nil
	})
	if err != nil {
		return sum, err
	}
	if _, ok := dests[storeName]; !ok {
		return sum, fmt.Errorf("%s holds no %s: not a psps backup", file, storeName)
	}

	err = entries(file, func(h *tar.Header, r io.Reader) error {
		dst := dests[h.Name]
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		tmp := dst + ".restoring"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, r)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(tmp, fs.FileMode(h.Mode).Perm())
		}
		if err == nil && h.Name == storeName {
			// A WAL left beside the old store would be replayed into
			// the new one.
			for _, x := range []string{"-wal", "-shm", "-journal"} {
				if rerr := os.Remove(dst + x); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					err = rerr
				}
			}
		}
		if err == nil {
			err = os.Rename(tmp, dst)
		}
		if err != nil {
			os.Remove(tmp)
			return err
		}
		os.Chtimes(dst, time.Now(), h.ModTime)
		sum.Files++
		sum.Bytes += n
		return nil
	})
	return sum, err
}

// destination is where an archive's entry goes in p, refusing a name that
// would land outside it.
func destination(p paths.Paths, name string) (string, error) {
	if name == storeName {
		return p.DB(), nil
	}
	clean := path.Clean(name)
	if clean != name || path.IsAbs(name) || strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("backup: refusing entry %q", name)
	}
	top, rest, ok := strings.Cut(name, "/")
	for _, r := range roots(p) {
		if ok && top == r[0] && rest != "" {
			return filepath.Join(r[1], filepath.FromSlash(rest)), nil
		}
	}
	return "", fmt.Errorf("backup: refusing entry %q", name)
}

// entries calls each with every file in the archive at file, refusing
// anything but plain files.
func entries(file string, each func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("backup: refusing entry %q: not a plain file", h.Name)
		}
		if err := each(h, tr); err != nil {
			return err
		}
	}
}
