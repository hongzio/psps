package isolation

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"reflect"
	"sort"
	"testing"
)

type entry struct {
	typ        byte
	name, link string
	body       string
}

func tarOf(t *testing.T, es ...entry) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range es {
		h := &tar.Header{Typeflag: e.typ, Name: e.name, Linkname: e.link, Mode: 0o644, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return &b
}

// names lists what a filtered stream holds, as "type name[->link]".
func names(t *testing.T, r io.Reader) []string {
	t.Helper()
	tr := tar.NewReader(r)
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		s := string(h.Typeflag) + " " + h.Name
		if h.Linkname != "" {
			s += "->" + h.Linkname
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func TestFilterTarKeepsTheTreeAndLeavesOutTheRest(t *testing.T) {
	in := tarOf(t,
		entry{typ: tar.TypeDir, name: "./"},
		entry{typ: tar.TypeDir, name: "./src/"},
		entry{typ: tar.TypeReg, name: "./src/a.go", body: "package a"},
		entry{typ: tar.TypeSymlink, name: "./src/in.go", link: "a.go"},
		entry{typ: tar.TypeSymlink, name: "./out", link: "/etc"},
		entry{typ: tar.TypeReg, name: "./out/passwd", body: "through the link"},
		entry{typ: tar.TypeLink, name: "./src/hard.go", link: "./src/a.go"},
		entry{typ: tar.TypeLink, name: "./src/hard2.go", link: "/etc/shadow"},
		entry{typ: tar.TypeFifo, name: "./fifo"},
		entry{typ: tar.TypeChar, name: "./dev"},
		entry{typ: tar.TypeDir, name: "./.git/"},
		entry{typ: tar.TypeReg, name: "./.git/config", body: "[core]"},
		entry{typ: tar.TypeReg, name: "./sub/.git", body: "gitdir: x"},
	)
	var out bytes.Buffer
	st, err := FilterTar(in, &out, CopyLimits{Exclude: []string{".git"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0 src/a.go", "1 src/hard.go->src/a.go", "2 out->/etc", "2 src/in.go->a.go", "5 src/"}
	if got := names(t, &out); !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	if st.Files != 1 || st.Bytes != 9 {
		t.Fatalf("stats %+v", st)
	}
	sort.Strings(st.Skipped)
	if want := []string{"dev", "fifo", "src/hard2.go"}; !reflect.DeepEqual(st.Skipped, want) {
		t.Fatalf("skipped %v, want %v", st.Skipped, want)
	}
}

func TestFilterTarRefusesNamesThatLeaveAndCopiesOverLimits(t *testing.T) {
	for _, n := range []string{"../x", "/etc/passwd", "a/../../x"} {
		if _, err := FilterTar(tarOf(t, entry{typ: tar.TypeReg, name: n, body: "x"}), io.Discard, CopyLimits{}); err == nil {
			t.Errorf("%q was copied", n)
		}
	}
	big := tarOf(t, entry{typ: tar.TypeReg, name: "a", body: "12345"}, entry{typ: tar.TypeReg, name: "b", body: "12345"})
	if _, err := FilterTar(big, io.Discard, CopyLimits{MaxBytes: 8}); !errors.Is(err, ErrCopyLimit) {
		t.Errorf("over the bytes: %v", err)
	}
	many := tarOf(t, entry{typ: tar.TypeReg, name: "a"}, entry{typ: tar.TypeReg, name: "b"}, entry{typ: tar.TypeReg, name: "c"})
	if _, err := FilterTar(many, io.Discard, CopyLimits{MaxFiles: 2}); !errors.Is(err, ErrCopyLimit) {
		t.Errorf("over the entries: %v", err)
	}
}

func TestParseJobState(t *testing.T) {
	for in, want := range map[string]JobState{"running": {Running: true}, "lost": {Lost: true}, "done 0": {}, "done 137": {Exit: 137}} {
		if got, err := parseJobState(in); err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	if _, err := parseJobState("done x"); err == nil {
		t.Error("done x parsed")
	}
}
