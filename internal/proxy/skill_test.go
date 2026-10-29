package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillFrontmatter reads every service's skill source: the agent CLIs
// parse the frontmatter as YAML and drop a skill whose frontmatter does not
// parse, silently for the agent. A plain scalar ends at " #" (a comment)
// and turns into a mapping at ": ", so neither may appear in a value.
func TestSkillFrontmatter(t *testing.T) {
	files, err := filepath.Glob("*/skill.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no skill sources found: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		_, rest, ok := strings.Cut(string(b), "`---\n")
		if !ok {
			t.Errorf("%s: no frontmatter", f)
			continue
		}
		front, _, ok := strings.Cut(rest, "\n---\n")
		if !ok {
			t.Errorf("%s: frontmatter not closed", f)
			continue
		}
		keys := map[string]bool{}
		for i, l := range strings.Split(front, "\n") {
			value := strings.TrimPrefix(l, "  ")
			if value == l {
				k, v, ok := strings.Cut(l, ": ")
				if !ok || k == "" || strings.ContainsAny(k, " \t") {
					t.Errorf("%s: frontmatter line %d is not `key: value`: %q", f, i+1, l)
					continue
				}
				keys[k], value = true, v
			}
			if strings.Contains(value, " #") || strings.Contains(value, ": ") || strings.HasSuffix(value, ":") {
				t.Errorf("%s: frontmatter line %d breaks a YAML plain scalar: %q", f, i+1, l)
			}
			if strings.ContainsAny(value[:1], "[]{}#&*!|>'\"%@`,") {
				t.Errorf("%s: frontmatter line %d starts with a YAML indicator: %q", f, i+1, l)
			}
		}
		if !keys["name"] || !keys["description"] {
			t.Errorf("%s: frontmatter needs name and description, has %v", f, keys)
		}
	}
}
