package skillproxy

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/hongzio/gero/internal/proxy"
)

// A skill is a folder with SKILL.md at its top: YAML frontmatter between
// --- lines, then the instructions, and whatever else it ships (scripts,
// references, assets) beside them. Claude Code and Codex both read it, and
// both drop a skill they cannot read without telling the agent, so a skill
// is checked here, as it is added, against what either of them needs: the
// Agent Skills format both follow, and Claude Code's own fields.

// Limits keep a skill something one sync carries.
const (
	maxFiles     = 256
	maxFileBytes = 1 << 20
	maxBytes     = 4 << 20
	maxDepth     = 8
)

// File is one of a skill's files, by its path inside the skill.
type File struct {
	Path    string
	Exec    bool
	Content []byte
}

// Text is whether a sync can carry the file as a JSON string.
func (f File) Text() bool { return utf8.Valid(f.Content) && !bytes.ContainsRune(f.Content, 0) }

type Skill struct {
	Name        string
	Description string
	Files       []File
	// Warnings are legal things the operator should hear about: a field
	// neither CLI reads, a link to a file the skill does not have.
	Warnings []string
	// source is the folder it was read from, links followed.
	source string
}

func (s Skill) Size() int {
	n := 0
	for _, f := range s.Files {
		n += len(f.Content)
	}
	return n
}

var nameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// CheckName is the Agent Skills rule for a name, which is also the folder's
// name: lowercase letters, digits and single hyphens, up to 64. gero keeps
// names starting with gero for its own skills, and Claude Code keeps
// anthropic-skills and synced.
func CheckName(name string) error {
	switch {
	case len(name) > 64 || !nameRE.MatchString(name):
		return proxy.Deny("%q is not a skill name: lowercase letters, digits and single hyphens, not at either end, up to 64", name)
	case strings.HasPrefix(name, "gero"):
		return proxy.Deny("%q: names starting with gero are kept for gero's own skills", name)
	case name == "synced", strings.HasPrefix(name, "anthropic-skills"):
		return proxy.Deny("%q is a name Claude Code keeps for its own", name)
	}
	return nil
}

// Load reads and checks the skill in dir, which must be called name.
func Load(dir, name string) (Skill, error) {
	sk := Skill{Name: name}
	st, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return sk, proxy.Deny("%s does not exist", dir)
	case err != nil:
		return sk, err
	case !st.IsDir():
		return sk, proxy.Deny("%s is not a folder: a skill is a folder with SKILL.md in it", dir)
	}
	if err := sk.read(dir, "", 0); err != nil {
		return sk, err
	}
	i := slices.IndexFunc(sk.Files, func(f File) bool { return f.Path == "SKILL.md" })
	if i < 0 {
		if j := slices.IndexFunc(sk.Files, func(f File) bool { return strings.EqualFold(f.Path, "SKILL.md") }); j >= 0 {
			return sk, proxy.Deny("%s: %s must be named SKILL.md, in capitals", dir, sk.Files[j].Path)
		}
		return sk, proxy.Deny("%s has no SKILL.md at its top: a skill is a folder with SKILL.md in it", dir)
	}
	if err := sk.parse(sk.Files[i]); err != nil {
		return sk, fmt.Errorf("%s/SKILL.md: %w", dir, err)
	}
	return sk, nil
}

// read takes in every file under dir/rel. Links are refused rather than
// followed: one could point at anything of the Mac's, which would then be
// handed to the agents.
func (sk *Skill) read(dir, rel string, depth int) error {
	if depth > maxDepth {
		return proxy.Deny("%s nests deeper than %d folders", path.Join(dir, rel), maxDepth)
	}
	entries, err := os.ReadDir(filepath.Join(dir, rel))
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		p := path.Join(rel, name)
		if name == ".DS_Store" || (name == ".git" && e.IsDir()) {
			continue
		}
		if !utf8.ValidString(name) || strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || r == '\\' }) {
			return proxy.Deny("%q in %s is not a usable file name", name, dir)
		}
		switch t := e.Type(); {
		case t&fs.ModeSymlink != 0:
			return proxy.Deny("%s is a link: a skill is copied as it is, so put the file itself there", path.Join(dir, p))
		case t.IsDir():
			if err := sk.read(dir, p, depth+1); err != nil {
				return err
			}
			continue
		case !t.IsRegular():
			return proxy.Deny("%s is not a file or a folder", path.Join(dir, p))
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxFileBytes {
			return proxy.Deny("%s is %d KiB; a skill's file may be up to %d KiB", path.Join(dir, p), info.Size()>>10, maxFileBytes>>10)
		}
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			return err
		}
		sk.Files = append(sk.Files, File{Path: p, Exec: info.Mode()&0o111 != 0, Content: b})
		if len(sk.Files) > maxFiles {
			return proxy.Deny("%s has more than %d files", dir, maxFiles)
		}
		if n := sk.Size(); n > maxBytes {
			return proxy.Deny("%s holds more than %d MiB", dir, maxBytes>>20)
		}
	}
	return nil
}

// fieldKind is what each frontmatter field Claude Code or the Agent Skills
// format reads must be.
type fieldKind int

const (
	text    fieldKind = iota // a string
	flag                     // a boolean
	words                    // a string or a list of strings
	mapping                  // anything, as a mapping
	labels                   // a mapping of strings to strings
)

var fields = map[string]fieldKind{
	"name": text, "description": text, "when_to_use": text, "license": text, "compatibility": text,
	"metadata": labels, "allowed-tools": words, "disallowed-tools": words, "argument-hint": text,
	"arguments": words, "disable-model-invocation": flag, "user-invocable": flag, "model": text,
	"effort": text, "context": text, "agent": text, "background": flag, "hooks": mapping,
	"paths": words, "shell": text,
}

var choices = map[string][]string{
	"context": {"fork"},
	"shell":   {"bash", "powershell"},
}

// Claude Code takes these, in any case, as booleans.
var bools = []string{"true", "false", "yes", "no", "on", "off", "1", "0"}

var (
	tagRE  = regexp.MustCompile(`<[A-Za-z/][^>]*>`)
	linkRE = regexp.MustCompile(`\]\(([^)\s]+)\)`)
)

func (sk *Skill) parse(f File) error {
	if !f.Text() {
		return proxy.Deny("not UTF-8 text")
	}
	src := strings.ReplaceAll(string(f.Content), "\r\n", "\n")
	if strings.HasPrefix(src, "\ufeff") {
		return proxy.Deny("it starts with a byte order mark; the CLIs read frontmatter only when --- is the very first line")
	}
	rest, ok := strings.CutPrefix(src, "---\n")
	if !ok {
		return proxy.Deny("it must start with a --- line opening its frontmatter (name, description)")
	}
	front, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		front, ok = strings.CutSuffix(rest, "\n---")
	}
	if !ok {
		return proxy.Deny("its frontmatter is not closed by a --- line")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(front), &doc); err != nil {
		return proxy.Deny("its frontmatter is not YAML the CLIs can read (a value with \": \" or \" #\" in it needs quotes): %v", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return proxy.Deny("its frontmatter must be a mapping of fields (name: ..., description: ...)")
	}
	m := doc.Content[0]
	got, seen := map[string]*yaml.Node{}, map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		key := k.Value
		if seen[key] {
			return proxy.Deny("its frontmatter has %s twice", key)
		}
		seen[key] = true
		kind, known := fields[key]
		if !known {
			sk.Warnings = append(sk.Warnings, fmt.Sprintf("SKILL.md: field %q is not one Claude Code or Codex reads; they ignore it", key))
			continue
		}
		if err := checkField(key, kind, v); err != nil {
			return err
		}
		got[key] = v
	}
	name, desc := got["name"], got["description"]
	switch {
	case name == nil:
		return proxy.Deny("its frontmatter has no name; Codex drops a skill without one (name: %s)", sk.Name)
	case name.Value != sk.Name:
		return proxy.Deny("its name is %q, but it is added as %q: the two must be the same, which is also its folder's name", name.Value, sk.Name)
	case desc == nil || strings.TrimSpace(desc.Value) == "":
		return proxy.Deny("its frontmatter has no description, which is how an agent knows when to use it")
	case utf8.RuneCountInString(desc.Value) > 1024:
		return proxy.Deny("its description is %d characters; up to 1024", utf8.RuneCountInString(desc.Value))
	}
	sk.Description = strings.Join(strings.Fields(desc.Value), " ")
	if c := got["compatibility"]; c != nil && utf8.RuneCountInString(c.Value) > 500 {
		return proxy.Deny("its compatibility is %d characters; up to 500", utf8.RuneCountInString(c.Value))
	}
	if tagRE.MatchString(desc.Value) {
		sk.Warnings = append(sk.Warnings, "SKILL.md: the description has an XML tag in it, which Claude's own skill uploads refuse")
	}
	if w := got["when_to_use"]; w != nil && utf8.RuneCountInString(desc.Value+w.Value) > 1536 {
		sk.Warnings = append(sk.Warnings, "SKILL.md: description and when_to_use run past 1536 characters, where Claude Code cuts them off")
	}
	if strings.TrimSpace(body) == "" {
		sk.Warnings = append(sk.Warnings, "SKILL.md has no instructions after its frontmatter")
	}
	if n := strings.Count(body, "\n"); n > 500 {
		sk.Warnings = append(sk.Warnings, fmt.Sprintf("SKILL.md is %d lines; keep it under 500 and move detail to files it links", n))
	}
	sk.checkLinks(body)
	return nil
}

func checkField(key string, kind fieldKind, v *yaml.Node) error {
	scalar := v.Kind == yaml.ScalarNode && v.Tag != "!!null"
	switch kind {
	case text:
		if !scalar {
			return proxy.Deny("%s must be a single value, as text", key)
		}
		if c, ok := choices[key]; ok && !slices.Contains(c, v.Value) {
			return proxy.Deny("%s is %q; it may be %s", key, v.Value, strings.Join(c, " or "))
		}
	case flag:
		if !scalar || !slices.Contains(bools, strings.ToLower(v.Value)) {
			return proxy.Deny("%s must be true or false", key)
		}
	case words:
		if scalar {
			return nil
		}
		if v.Kind != yaml.SequenceNode {
			return proxy.Deny("%s must be text or a list of text", key)
		}
		for _, x := range v.Content {
			if x.Kind != yaml.ScalarNode || x.Tag == "!!null" {
				return proxy.Deny("%s must be text or a list of text", key)
			}
		}
	case mapping:
		if v.Kind != yaml.MappingNode {
			return proxy.Deny("%s must be a mapping", key)
		}
	case labels:
		if v.Kind != yaml.MappingNode {
			return proxy.Deny("%s must be a mapping of names to text", key)
		}
		for _, x := range v.Content {
			if x.Kind != yaml.ScalarNode {
				return proxy.Deny("%s must be a mapping of names to text", key)
			}
		}
	}
	return nil
}

// checkLinks warns of a relative link in SKILL.md to a file the skill does
// not have: the agent would follow it and find nothing.
func (sk *Skill) checkLinks(body string) {
	have := map[string]bool{}
	for _, f := range sk.Files {
		for d := f.Path; d != "."; d = path.Dir(d) {
			have[d] = true
		}
	}
	seen := map[string]bool{}
	for _, m := range linkRE.FindAllStringSubmatch(body, -1) {
		target, _, _ := strings.Cut(m[1], "#")
		if target == "" || strings.Contains(target, ":") || strings.HasPrefix(target, "/") || seen[target] {
			continue
		}
		seen[target] = true
		if p := path.Clean(target); !have[p] {
			sk.Warnings = append(sk.Warnings, fmt.Sprintf("SKILL.md links %s, which the skill does not have", target))
		}
	}
}
