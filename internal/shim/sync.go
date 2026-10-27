package shim

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/hongzio/gero/internal/shimproto"
)

// Managed says whether gero may write or remove path. Everything gero puts
// in an isolation has a path element starting with "gero" (/etc/gero/...,
// ~/.claude/skills/gero-git/...), so a mistake in the daemon cannot reach a
// file the user or an agent made.
func Managed(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, el := range strings.Split(path, "/") {
		if strings.HasPrefix(el, "gero") {
			return true
		}
	}
	return false
}

// Sync makes req true: each file written (only if it differs) and each
// removal done. Files marked Agent, and any directory created for them, are
// owned by uid:gid; the rest by root.
func Sync(req shimproto.SyncRequest, uid, gid int) (shimproto.SyncResult, error) {
	var res shimproto.SyncResult
	for _, f := range req.Files {
		if !Managed(f.Path) {
			return res, fmt.Errorf("sync: %q is not a path gero manages", f.Path)
		}
	}
	for _, p := range req.Remove {
		if !Managed(p) {
			return res, fmt.Errorf("sync: %q is not a path gero manages", p)
		}
	}
	var errs []error
	for _, f := range req.Files {
		owner := [2]int{0, 0}
		if f.Agent {
			owner = [2]int{uid, gid}
		}
		changed, err := writeFile(f, owner)
		if err != nil {
			errs = append(errs, err)
		} else if changed {
			res.Written = append(res.Written, f.Path)
		}
	}
	for _, p := range req.Remove {
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
		} else {
			res.Removed = append(res.Removed, p)
		}
	}
	return res, errors.Join(errs...)
}

func writeFile(f shimproto.SyncFile, owner [2]int) (bool, error) {
	mode := fs.FileMode(f.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	if st, err := os.Lstat(f.Path); err == nil && st.Mode().IsRegular() && st.Mode().Perm() == mode {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) == owner[0] && int(sys.Gid) == owner[1] {
			if b, err := os.ReadFile(f.Path); err == nil && bytes.Equal(b, []byte(f.Content)) {
				return false, nil
			}
		}
	}
	if err := mkdirs(filepath.Dir(f.Path), owner); err != nil {
		return false, err
	}
	tmp := filepath.Join(filepath.Dir(f.Path), ".gero-sync-"+strconv.Itoa(os.Getpid())+"-"+filepath.Base(f.Path))
	if err := os.WriteFile(tmp, []byte(f.Content), mode); err != nil {
		return false, err
	}
	// WriteFile's mode is filtered by the umask.
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err := os.Lchown(tmp, owner[0], owner[1]); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, f.Path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// mkdirs creates dir and any missing parents, giving the new ones to owner:
// ~/.claude/skills may not exist yet, and the agent must own what is made
// in its home.
func mkdirs(dir string, owner [2]int) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append(missing, d)
		if d == filepath.Dir(d) {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := os.Lchown(missing[i], owner[0], owner[1]); err != nil {
			return err
		}
	}
	return nil
}
