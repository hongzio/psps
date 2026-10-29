package isolation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeContainer is a `container` that writes each argv it is run with, one
// line each, to the file it returns.
func fakeContainer(t *testing.T) (CLI, string) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	bin := filepath.Join(dir, "container")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return CLI{Bin: bin}, log
}

func TestDNSReachesRunAndBuild(t *testing.T) {
	c, log := fakeContainer(t)
	c.DNS = []string{"1.1.1.1", "8.8.8.8"}
	if err := c.Run(context.Background(), RunSpec{Name: "gero-x", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Build(context.Background(), "gero-base:x", "/ctx", nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{
		"run -d --init --name gero-x --dns 1.1.1.1 --dns 8.8.8.8 img",
		"build --progress plain --dns 1.1.1.1 --dns 8.8.8.8 -t gero-base:x /ctx",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ran\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestNoDNSLeavesContainersOwn(t *testing.T) {
	c, log := fakeContainer(t)
	if err := c.Run(context.Background(), RunSpec{Name: "gero-x", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "--dns") {
		t.Fatalf("ran %s", b)
	}
}
