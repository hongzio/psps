package creds

// Values in the credentials file are sealed with a key kept elsewhere, so a
// stray `cat`, a screen share or a dotfiles commit of ~/.config shows
// ciphertext. It is not a defence against a process running as the user:
// the key is a file that user can read, as the daemon must.
//
// A sealed value is `enc:v1:` and base64 of a 12-byte nonce then AES-256-GCM
// ciphertext, with the section and field as additional data, so a value
// moved under another section or field fails to open rather than serving
// as the wrong secret. A value without the prefix is read as it is, and
// Plain names where those are left.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const sealedPrefix = "enc:v1:"

// Sealed says whether a value as written in the file is encrypted.
func Sealed(v string) bool { return strings.HasPrefix(v, sealedPrefix) }

// readKey loads the key, or returns os.ErrNotExist when there is none. A key
// group or other can read is refused as the file is.
func (s *Store) readKey() ([]byte, error) {
	if s.KeyPath == "" {
		return nil, &Denied{"psps was started without a credentials key path"}
	}
	st, err := os.Stat(s.KeyPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err != nil {
		return nil, &Denied{fmt.Sprintf("cannot read %s: %v", s.KeyPath, err)}
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, &Denied{fmt.Sprintf("%s is readable by group or other; it opens the credentials file — run `chmod 600 %s`", s.KeyPath, s.KeyPath)}
	}
	b, err := os.ReadFile(s.KeyPath)
	if err != nil {
		return nil, &Denied{fmt.Sprintf("cannot read %s: %v", s.KeyPath, err)}
	}
	if len(b) == 32 {
		return b, nil
	}
	// The first build wrote the key as a base64 line; one made by it in
	// the moment before an upgrade still opens what it sealed.
	if key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b))); err == nil && len(key) == 32 {
		return key, nil
	}
	return nil, &Denied{fmt.Sprintf("%s is not a credentials key (32 bytes)", s.KeyPath)}
}

// key returns the key, making one when there is none yet: only a write
// does, and only before anything is sealed with it.
func (s *Store) key(create bool) ([]byte, error) {
	key, err := s.readKey()
	if !errors.Is(err, os.ErrNotExist) || !create {
		return key, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(s.KeyPath), 0o700); err != nil {
		return nil, err
	}
	// O_EXCL: two writers racing to make the key keep the first one's.
	f, err := os.OpenFile(s.KeyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return s.readKey()
	}
	if err != nil {
		return nil, err
	}
	// Raw bytes, not text: a stray cat or editor shows unprintable noise
	// that cannot be copied or read off a screen as the key.
	_, werr := f.Write(key)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(s.KeyPath)
		return nil, werr
	}
	return key, nil
}

func aead(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func ad(sec, field string) []byte { return []byte(sec + "\x00" + field) }

func seal(key []byte, sec, field, v string) (string, error) {
	g, err := aead(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := g.Seal(nonce, nonce, []byte(v), ad(sec, field))
	return sealedPrefix + base64.StdEncoding.EncodeToString(out), nil
}

func open(key []byte, sec, field, v string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, sealedPrefix))
	g, gerr := aead(key)
	if gerr != nil {
		return "", gerr
	}
	if err != nil || len(raw) < g.NonceSize() {
		return "", fmt.Errorf("%s in [%s] is not a sealed value", field, sec)
	}
	pt, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], ad(sec, field))
	if err != nil {
		return "", fmt.Errorf("%s in [%s] does not open with this key", field, sec)
	}
	return string(pt), nil
}

// unseal opens every sealed value of f in place. The key is read only when
// something is sealed, so a file of plain values needs none.
func (s *Store) unseal(f *file) error {
	var key []byte
	for _, sec := range f.order {
		for k, v := range f.sections[sec] {
			if !Sealed(v) {
				continue
			}
			if key == nil {
				var err error
				if key, err = s.readKey(); errors.Is(err, os.ErrNotExist) {
					return &Denied{fmt.Sprintf("%s holds encrypted values but their key %s is missing; restore it from a backup, or set each secret again with `psps env secret`", s.Path, s.KeyPath)}
				} else if err != nil {
					return err
				}
			}
			pt, err := open(key, sec, k, v)
			if err != nil {
				return &Denied{fmt.Sprintf("%s: %v; was %s replaced? set it again with `psps env secret`", s.Path, err, s.KeyPath)}
			}
			f.sections[sec][k] = pt
		}
	}
	return nil
}

// Plain names the sections still holding a value in plain text, without
// reading any key or value: what verify warns of. A missing file has none.
func (s *Store) Plain() ([]string, error) {
	f, err := s.loadRaw()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sec := range f.order {
		for _, v := range f.sections[sec] {
			if !Sealed(v) {
				out = append(out, sec)
				break
			}
		}
	}
	return out, nil
}

// Encrypt rewrites the file with every value sealed, making the key if
// there is none, and says how many sections held plain values. A missing
// file is left missing.
func (s *Store) Encrypt() (int, error) {
	plain, err := s.Plain()
	if err != nil || len(plain) == 0 {
		return 0, err
	}
	err = s.update(false, func(*file) error { return nil })
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return len(plain), err
}

// TestStore is a store at path with its key beside it, for tests, which
// have no state directory to keep it apart in.
func TestStore(path string) *Store { return &Store{Path: path, KeyPath: path + ".key"} }
