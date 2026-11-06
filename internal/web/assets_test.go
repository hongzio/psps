package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The page's scripts are modules: one that does not parse as a module
// leaves the page blank, which a browser says only in its console. Node
// parses each the way a browser does, where it is installed.
func TestThePagesScriptsParseAsModules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node to parse the scripts with")
	}
	for _, name := range []string{"app.js", "icons.js"} {
		f, err := os.Open(filepath.Join("static", "assets", name))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(node, "--input-type=module", "--check")
		cmd.Stdin = f
		out, err := cmd.CombinedOutput()
		f.Close()
		if err != nil {
			t.Errorf("%s does not parse as a module: %v\n%s", name, err, out)
		}
	}
}
