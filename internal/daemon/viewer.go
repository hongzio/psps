package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/store"
)

// The web's viewer reads files in an agent's isolation, as the agent: any
// it could read, in its workspace or not (/tmp, say), never one it could
// not. It only reads.

// statFmt prints a file's size and modification time, in seconds: GNU
// stat's way in an isolation, BSD's on the Mac.
const statFmt = `stat -c '%s %Y' "$f" 2>/dev/null || stat -f '%z %m' "$f" 2>/dev/null`

// fileScript prints what $1 is on its first line, and, if a file, its
// size and time there and its first $2 bytes in base64 after.
const fileScript = `f=$1
if [ ! -e "$f" ]; then echo psps:none; exit 0; fi
if [ -d "$f" ]; then echo psps:dir; exit 0; fi
if [ ! -f "$f" ]; then echo psps:special; exit 0; fi
if [ ! -r "$f" ]; then echo psps:unreadable; exit 0; fi
s=$(` + statFmt + `) || exit 1
echo "psps:ok $s"
head -c "$2" "$f" | base64`

// statScript prints, for each of its arguments that is a file, its index
// among them, its size and its time.
const statScript = `i=0
for f; do
	if [ -f "$f" ] && s=$(` + statFmt + `); then echo "psps:$i $s"; fi
	i=$((i+1))
done
exit 0`

// statMax is how many paths one stat asks of.
const statMax = 200

var imageTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".svg": "image/svg+xml", ".avif": "image/avif", ".bmp": "image/bmp", ".ico": "image/x-icon",
}

// textTypes are the types of text files the viewer shows a way of its
// own, which the system's table may not know.
var textTypes = map[string]string{
	".md": "text/markdown", ".markdown": "text/markdown", ".html": "text/html", ".htm": "text/html",
}

// viewerAgent is an agent whose isolation files are read in, and the
// container and home there.
func (m *Manager) viewerAgent(id string) (store.Agent, string, string, error) {
	a, err := m.storedAgent(id)
	if err != nil {
		return a, "", "", err
	}
	iso, err := m.store.Isolation(a.IsolationID)
	if err != nil {
		return a, "", "", err
	}
	home := m.home
	if iso.Container == store.HostContainer {
		if home, err = os.UserHomeDir(); err != nil {
			return a, "", "", err
		}
	}
	return a, iso.Container, home, nil
}

// agentPath is p whole in the agent's isolation: relative, it is to its
// workdir; ~ is the home there.
func agentPath(a store.Agent, home, p string) (string, error) {
	switch {
	case strings.ContainsAny(p, "\x00\n"):
		return "", errors.New("a path has no NUL or newline in it")
	case strings.TrimSpace(p) == "":
		return "", errors.New("which file? a path is needed")
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = home + p[1:]
	case !path.IsAbs(p):
		p = path.Join(a.Workdir, p)
	}
	return path.Clean(p), nil
}

// File reads a file in the agent's isolation for the viewer: its text, the
// first api.FileTextMax bytes of it, or an image whole.
func (m *Manager) File(ctx context.Context, id, p string) (api.File, error) {
	a, container, home, err := m.viewerAgent(id)
	if err != nil {
		return api.File{}, err
	}
	full, err := agentPath(a, home, p)
	if err != nil {
		return api.File{}, badRequest(err)
	}
	ext := strings.ToLower(path.Ext(full))
	typ, image := imageTypes[ext]
	limit := api.FileTextMax
	if image {
		limit = api.FileImageMax
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := m.cli.RunAsAgent(ctx, container, "sh", "-c", fileScript, "sh", full, strconv.Itoa(limit+1))
	if err != nil {
		return api.File{}, fmt.Errorf("reading %s: %w", full, err)
	}
	head, body, _ := strings.Cut(out, "\n")
	switch head {
	case "psps:none":
		return api.File{}, fmt.Errorf("%s: %w", full, store.ErrNotFound)
	case "psps:dir":
		return api.File{}, badRequest(fmt.Errorf("%s is a folder", full))
	case "psps:special":
		return api.File{}, badRequest(fmt.Errorf("%s is not a file", full))
	case "psps:unreadable":
		return api.File{}, httpError{http.StatusForbidden, fmt.Errorf("%s: the agent may not read it", full)}
	}
	var size, mtime int64
	if _, err := fmt.Sscanf(head, "psps:ok %d %d", &size, &mtime); err != nil {
		return api.File{}, fmt.Errorf("reading %s: %s", full, out)
	}
	f := api.File{Path: full, Size: size, MTime: time.Unix(mtime, 0)}
	if image && size > api.FileImageMax {
		f.Kind, f.Type = api.FileTooLarge, typ
		return f, nil
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(body), ""))
	if err != nil {
		return api.File{}, fmt.Errorf("reading %s: %w", full, err)
	}
	if image {
		f.Kind, f.Type, f.Data = api.FileImage, typ, base64.StdEncoding.EncodeToString(data)
		return f, nil
	}
	if len(data) > api.FileTextMax {
		data, f.Truncated = data[:api.FileTextMax], true
		// Cut, it may end in part of a character.
		for i := 0; i < utf8.UTFMax && len(data) > 0 && !utf8.Valid(data); i++ {
			data = data[:len(data)-1]
		}
	}
	f.Type = textTypes[ext]
	if f.Type == "" {
		f.Type = mime.TypeByExtension(ext)
	}
	if f.Type == "" {
		f.Type = http.DetectContentType(data)
	}
	if strings.IndexByte(string(data), 0) >= 0 || !utf8.Valid(data) {
		f.Kind = api.FileBinary
		return f, nil
	}
	f.Kind, f.Text = api.FileText, string(data)
	return f, nil
}

// StatFiles says which of paths are files in the agent's isolation, with
// their sizes and times: for the web to link a path a message names only
// when it is one, and to see one it shows change.
func (m *Manager) StatFiles(ctx context.Context, id string, paths []string) (api.FileStats, error) {
	if len(paths) > statMax {
		return api.FileStats{}, badRequest(fmt.Errorf("at most %d paths at once", statMax))
	}
	a, container, home, err := m.viewerAgent(id)
	if err != nil {
		return api.FileStats{}, err
	}
	out := api.FileStats{Files: []api.FileStat{}}
	var asked, full []string
	for _, p := range paths {
		if f, err := agentPath(a, home, p); err == nil {
			asked, full = append(asked, p), append(full, f)
		}
	}
	if len(full) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := m.cli.RunAsAgent(ctx, container, append([]string{"sh", "-c", statScript, "sh"}, full...)...)
	if err != nil {
		return api.FileStats{}, fmt.Errorf("looking for files: %w", err)
	}
	for l := range strings.SplitSeq(res, "\n") {
		var i int
		var size, mtime int64
		if n, _ := fmt.Sscanf(l, "psps:%d %d %d", &i, &size, &mtime); n != 3 || i < 0 || i >= len(full) {
			continue
		}
		out.Files = append(out.Files, api.FileStat{Path: asked[i], Abs: full[i], Size: size, MTime: time.Unix(mtime, 0)})
	}
	return out, nil
}

// servePage serves an HTML file of the agent's as a page for the viewer's
// frame: in a sandbox of its own origin, which reaches nothing outside it
// (it can show only what it has inline), and runs its scripts only when
// asked to.
func (m *Manager) servePage(w http.ResponseWriter, r *http.Request) {
	f, err := m.File(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err == nil && f.Kind != api.FileText {
		err = badRequest(fmt.Errorf("%s is not text", f.Path))
	}
	if err != nil {
		reply(w, nil, err)
		return
	}
	sandbox, scripts := "sandbox", "'none'"
	if r.URL.Query().Get("scripts") == "1" {
		sandbox, scripts = "sandbox allow-scripts", "'unsafe-inline'"
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", sandbox+"; default-src 'none'; script-src "+scripts+
		"; style-src 'unsafe-inline' data:; img-src data:; font-src data:; media-src data:; frame-ancestors 'self'")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Cache-Control", "no-store")
	w.Write([]byte(f.Text))
}
