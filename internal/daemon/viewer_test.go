package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/store"
)

func TestViewerFiles(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	write := func(p string, b []byte) {
		t.Helper()
		if !filepath.IsAbs(p) {
			p = filepath.Join(a.Workdir, p)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/plan.md", []byte("# Plan\n\n한국어도 된다\n"))
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	write("img/dot.png", png)
	write("bin/blob", []byte("ab\x00cd"))
	outside := filepath.Join(t.TempDir(), "report.html")
	write(outside, []byte("<p>hi</p>"))

	f, err := m.File(ctx, a.ID, "docs/plan.md")
	if err != nil || f.Kind != api.FileText || f.Text != "# Plan\n\n한국어도 된다\n" || f.Type != "text/markdown" ||
		f.Path != filepath.Join(a.Workdir, "docs/plan.md") || f.Size != int64(len("# Plan\n\n한국어도 된다\n")) || f.MTime.IsZero() {
		t.Fatalf("%+v %v", f, err)
	}
	// Anywhere in the isolation, not only the workspace.
	if f, err := m.File(ctx, a.ID, outside); err != nil || f.Text != "<p>hi</p>" || f.Type != "text/html" {
		t.Fatalf("%+v %v", f, err)
	}
	if f, err := m.File(ctx, a.ID, "img/dot.png"); err != nil || f.Kind != api.FileImage || f.Type != "image/png" ||
		f.Data != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("%+v %v", f, err)
	}
	if f, err := m.File(ctx, a.ID, "bin/blob"); err != nil || f.Kind != api.FileBinary || f.Text != "" {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := m.File(ctx, a.ID, "nope.md"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a missing file: %v", err)
	}
	if _, err := m.File(ctx, a.ID, "docs"); err == nil {
		t.Fatal("a folder was read")
	}

	// Text is cut, on a character, at FileTextMax; an image larger is not sent.
	write("big.txt", []byte(strings.Repeat("a", api.FileTextMax-1)+"한"+"tail"))
	if f, err := m.File(ctx, a.ID, "big.txt"); err != nil || !f.Truncated || f.Kind != api.FileText || len(f.Text) != api.FileTextMax-1 {
		t.Fatalf("%d %v %v", len(f.Text), f.Truncated, err)
	}
	write("huge.png", make([]byte, api.FileImageMax+1))
	if f, err := m.File(ctx, a.ID, "huge.png"); err != nil || f.Kind != api.FileTooLarge || f.Data != "" {
		t.Fatalf("%+v %v", f.Kind, err)
	}

	st, err := m.StatFiles(ctx, a.ID, []string{"docs/plan.md", "nope", "docs", outside, "a\nb"})
	if err != nil || len(st.Files) != 2 || st.Files[0].Path != "docs/plan.md" || st.Files[0].Abs != filepath.Join(a.Workdir, "docs/plan.md") ||
		st.Files[1].Path != outside || st.Files[1].Size != 9 {
		t.Fatalf("%+v %v", st, err)
	}

	// An HTML file as a page: sandboxed, its scripts on only when asked.
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	for _, scripts := range []bool{false, true} {
		u := srv.URL + "/v1/agents/" + a.ID + "/page?path=" + outside
		if scripts {
			u += "&scripts=1"
		}
		res, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		csp := res.Header.Get("Content-Security-Policy")
		res.Body.Close()
		if res.StatusCode != 200 || !strings.HasPrefix(csp, "sandbox") || strings.Contains(csp, "allow-same-origin") ||
			strings.Contains(csp, "allow-scripts") != scripts || !strings.Contains(csp, "default-src 'none'") {
			t.Fatalf("%d %q", res.StatusCode, csp)
		}
	}

	// Downloaded whole and as bytes, whatever it is, past what is shown.
	for _, c := range []struct {
		path string
		want []byte
	}{{"bin/blob", []byte("ab\x00cd")}, {"big.txt", []byte(strings.Repeat("a", api.FileTextMax-1) + "한" + "tail")}} {
		res, err := http.Get(srv.URL + "/v1/agents/" + a.ID + "/download?path=" + c.path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || string(b) != string(c.want) || res.Header.Get("Content-Type") != "application/octet-stream" ||
			res.Header.Get("Content-Disposition") != "attachment; filename="+filepath.Base(c.path) {
			t.Fatalf("%s: %d %q %d bytes", c.path, res.StatusCode, res.Header.Get("Content-Disposition"), len(b))
		}
	}
	for path, code := range map[string]int{"nope.md": 404, "docs": 400} {
		res, err := http.Get(srv.URL + "/v1/agents/" + a.ID + "/download?path=" + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != code {
			t.Fatalf("%s: %d", path, res.StatusCode)
		}
	}
}
