package skillproxy

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hongzio/psps/internal/proxy"
)

// A skill can come from a public GitHub repository instead of a folder of
// the Mac's, as `npx skills add owner/repo` takes one: the daemon fetches
// it, checks it as it would check a folder, and keeps it the same way. Where
// it came from is kept in its env as its Origin, so it can be checked
// against what the repository has now and updated, by the operator only:
// a skill's scripts run as the agent, and a push to someone's repository
// does not reach the isolations on its own.

const (
	githubAPI = "https://api.github.com"
	githubRaw = "https://raw.githubusercontent.com"
	// A repository's tarball is read as it arrives, up to these.
	maxTarball  = 64 << 20
	maxUnpacked = 512 << 20
	// A repository is searched for up to this many SKILL.md files.
	maxCandidates = 100
	fetchTimeout  = 90 * time.Second
)

// Origin is where a skill kept from GitHub came from, as its env keeps it.
type Origin struct {
	Repo string `json:"repo"`
	// Ref is the branch, tag or commit it follows.
	Ref string `json:"ref"`
	// Path is the skill's folder in the repository; empty, its top.
	Path   string `json:"path,omitempty"`
	Commit string `json:"commit"`
	// Tree is the git tree of Path at Commit: what changes when, and
	// only when, the skill's own files do.
	Tree string `json:"tree"`
	// Digest is the kept copy's files as fetched, to tell an edit made
	// on the Mac since.
	Digest    string    `json:"digest"`
	FetchedAt time.Time `json:"fetched_at"`
}

func (o Origin) where() string {
	return strings.TrimSuffix(o.Repo+"/"+o.Path, "/") + " @ " + o.Ref
}

// Pinned is an origin whose ref is a commit: there is nothing to follow.
func (o Origin) Pinned() bool {
	return shaRE.MatchString(o.Ref) && strings.HasPrefix(o.Commit, strings.ToLower(o.Ref))
}

func originOf(env map[string]any) (Origin, bool) {
	var o Origin
	raw, ok := env["origin"]
	if !ok || raw == nil {
		return o, false
	}
	b, _ := json.Marshal(raw)
	if json.Unmarshal(b, &o) != nil || o.Repo == "" {
		return o, false
	}
	return o, true
}

func (o Origin) env() map[string]any {
	b, _ := json.Marshal(o)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

// Spec is a GitHub source as typed: a repository, and maybe a ref and the
// skill's folder in it.
type Spec struct {
	Repo string
	Ref  string
	Path string
	// Exact: Path names the folder (even the top, empty), so nothing is
	// searched for.
	Exact bool
	// refPath is what follows /tree/ or /blob/ in an address, a ref and
	// then a path, split only once the repository says which refs it has.
	refPath []string
	blob    bool
}

var (
	ownerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	shaRE   = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
)

// ParseSpec reads owner/repo, owner/repo/PATH, owner/repo@REF, and
// github.com addresses, those ending /tree/REF/PATH or /blob/REF/PATH too.
func ParseSpec(s string) (Spec, error) {
	in := strings.TrimSpace(s)
	bad := func(why string) (Spec, error) {
		return Spec{}, proxy.Deny("%q is not a GitHub source (%s): give owner/repo, owner/repo/PATH or a github.com address", in, why)
	}
	if strings.HasPrefix(in, "git@") || strings.HasPrefix(in, "ssh://") {
		return bad("psps fetches over HTTPS, so not an SSH address")
	}
	v := in
	if i := strings.Index(v, "://"); i >= 0 {
		scheme := strings.ToLower(v[:i])
		if scheme != "https" && scheme != "http" {
			return bad("not an http address")
		}
		v = v[i+3:]
	}
	v, _, _ = strings.Cut(v, "#")
	v, _, _ = strings.Cut(v, "?")
	host := ""
	if first, rest, ok := strings.Cut(v, "/"); ok && strings.Contains(first, ".") {
		host, v = strings.ToLower(first), rest
	} else if strings.Contains(first, ".") && !strings.Contains(v, "/") {
		return bad("no repository")
	}
	if host != "" && host != "github.com" && host != "www.github.com" {
		return bad("only github.com")
	}
	v = strings.Trim(v, "/")
	var sp Spec
	if i := strings.LastIndex(v, "@"); i >= 0 {
		sp.Ref = v[i+1:]
		v = v[:i]
		if sp.Ref == "" {
			return bad("nothing after @")
		}
	}
	segs := strings.Split(v, "/")
	if len(segs) < 2 {
		return bad("no repository")
	}
	owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
	if !ownerRE.MatchString(owner) || !repoRE.MatchString(repo) || repo == "." || repo == ".." {
		return bad("owner/repo is not a GitHub repository's name")
	}
	sp.Repo = owner + "/" + repo
	rest := segs[2:]
	for _, seg := range rest {
		if seg == "" || seg == "." || seg == ".." {
			return bad("a path with an empty, . or .. part")
		}
	}
	if len(rest) > 0 && (rest[0] == "tree" || rest[0] == "blob") && host != "" {
		if sp.Ref != "" {
			return bad("a ref both after @ and in the address")
		}
		if len(rest) < 2 {
			return bad("nothing after /" + rest[0])
		}
		sp.blob = rest[0] == "blob"
		sp.refPath = rest[1:]
		sp.Exact = true
		return sp, nil
	}
	if len(rest) > 0 {
		sp.Path, sp.Exact = strings.Join(rest, "/"), true
		if path.Base(sp.Path) == "SKILL.md" {
			sp.Path = strings.TrimSuffix(strings.TrimSuffix(sp.Path, "SKILL.md"), "/")
		}
	}
	return sp, nil
}

// github is the GitHub API and raw files, unauthenticated: public
// repositories only.
type github struct {
	api, raw string
	c        *http.Client
}

func (s Service) github() github {
	g := github{api: s.API, raw: s.Raw, c: s.Client}
	if g.api == "" {
		g.api = githubAPI
	}
	if g.raw == "" {
		g.raw = githubRaw
	}
	if g.c == nil {
		g.c = &http.Client{Timeout: fetchTimeout}
	}
	return g
}

// errMissing is GitHub's 404 (or 422, a ref it cannot read): no such
// repository, ref or path, or a private one.
var errMissing = errors.New("not found")

func escPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// open GETs u and answers the response when it is 200.
func (g github) open(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "psps")
	if strings.HasPrefix(u, g.api) {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	res, err := g.c.Do(req)
	if err != nil {
		return nil, proxy.Deny("GitHub could not be reached: %v", err)
	}
	if res.StatusCode == http.StatusOK {
		return res, nil
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	switch res.StatusCode {
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		return nil, errMissing
	case http.StatusForbidden, http.StatusTooManyRequests:
		if res.Header.Get("X-RateLimit-Remaining") == "0" {
			when := "in a while"
			if n, err := strconv.ParseInt(res.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				when = "at " + time.Unix(n, 0).Local().Format("15:04")
			}
			return nil, proxy.Deny("GitHub's limit for unauthenticated requests from this Mac is used up; it comes back %s", when)
		}
	}
	return nil, proxy.Deny("GitHub answered %s for %s", res.Status, strings.TrimPrefix(strings.TrimPrefix(u, g.api), g.raw))
}

func (g github) getJSON(ctx context.Context, p string, into any) error {
	res, err := g.open(ctx, g.api+p)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(into); err != nil {
		return proxy.Deny("GitHub's answer for %s could not be read: %v", p, err)
	}
	return nil
}

func missingRepo(repo string) error {
	return proxy.Deny("github.com/%s does not exist or is private: psps fetches skills from public repositories only", repo)
}

func (g github) defaultBranch(ctx context.Context, repo string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.getJSON(ctx, "/repos/"+repo, &r); err != nil {
		if errors.Is(err, errMissing) {
			return "", missingRepo(repo)
		}
		return "", err
	}
	if r.DefaultBranch == "" {
		return "", proxy.Deny("github.com/%s has no branch yet", repo)
	}
	return r.DefaultBranch, nil
}

// commit is the commit ref names now, and its top tree.
func (g github) commit(ctx context.Context, repo, ref string) (sha, tree string, err error) {
	var r struct {
		SHA    string `json:"sha"`
		Commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
	}
	if err := g.getJSON(ctx, "/repos/"+repo+"/commits/"+escPath(ref), &r); err != nil {
		return "", "", err
	}
	if r.SHA == "" || r.Commit.Tree.SHA == "" {
		return "", "", proxy.Deny("GitHub gave no commit for %s @ %s", repo, ref)
	}
	return r.SHA, r.Commit.Tree.SHA, nil
}

// tree is the git tree of p at commit sha, whose top tree is top.
func (g github) tree(ctx context.Context, repo, sha, top, p string) (string, error) {
	if p == "" {
		return top, nil
	}
	parent, base := path.Dir(p), path.Base(p)
	if parent == "." {
		parent = ""
	}
	var entries []struct {
		Name, Type, SHA string
	}
	if err := g.getJSON(ctx, "/repos/"+repo+"/contents/"+escPath(parent)+"?ref="+sha, &entries); err != nil {
		if errors.Is(err, errMissing) {
			return "", proxy.Deny("%s has no folder %s at %s", repo, p, short(sha))
		}
		return "", err
	}
	for _, e := range entries {
		if e.Name == base && e.Type == "dir" {
			return e.SHA, nil
		}
	}
	return "", proxy.Deny("%s has no folder %s at %s", repo, p, short(sha))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// resolved is a Spec with its ref, path and commit settled.
type resolved struct {
	Spec
	commit, top string
}

// resolve settles sp's ref and the commit it names now: the default
// branch when it names none, and for an address's /tree/ part the
// shortest start of it that is a ref (a branch may have / in its name).
// ref, when given, replaces the spec's own.
func (g github) resolve(ctx context.Context, sp Spec, ref string) (resolved, error) {
	if len(sp.refPath) > 0 {
		found := false
		for i := 1; i <= len(sp.refPath); i++ {
			cand := strings.Join(sp.refPath[:i], "/")
			sha, top, err := g.commit(ctx, sp.Repo, cand)
			if errors.Is(err, errMissing) {
				continue
			}
			if err != nil {
				return resolved{}, err
			}
			sp.Ref, sp.Path = cand, strings.Join(sp.refPath[i:], "/")
			if sp.blob {
				sp.Path = path.Dir(sp.Path)
				if sp.Path == "." {
					sp.Path = ""
				}
			}
			found = true
			if ref == "" {
				return resolved{Spec: sp, commit: sha, top: top}, nil
			}
			break
		}
		if !found {
			if _, err := g.defaultBranch(ctx, sp.Repo); err != nil {
				return resolved{}, err
			}
			return resolved{}, proxy.Deny("github.com/%s has no branch, tag or commit %s", sp.Repo, sp.refPath[0])
		}
		sp.refPath = nil
	}
	if ref != "" {
		sp.Ref = ref
	}
	if sp.Ref == "" {
		b, err := g.defaultBranch(ctx, sp.Repo)
		if err != nil {
			return resolved{}, err
		}
		sp.Ref = b
	}
	sha, top, err := g.commit(ctx, sp.Repo, sp.Ref)
	if errors.Is(err, errMissing) {
		if _, err := g.defaultBranch(ctx, sp.Repo); err != nil {
			return resolved{}, err
		}
		return resolved{}, proxy.Deny("github.com/%s has no branch, tag or commit %s", sp.Repo, sp.Ref)
	}
	if err != nil {
		return resolved{}, err
	}
	return resolved{Spec: sp, commit: sha, top: top}, nil
}

// Found is a skill a repository has: where, and what its SKILL.md says.
type Found struct {
	Path        string
	Name        string
	Description string
	rank        int
}

// rank is where a SKILL.md is, as the places skills are kept come first;
// -1 is nowhere a skill is looked for.
func rank(dir string) int {
	if dir == "" {
		return 0
	}
	segs := strings.Split(dir, "/")
	for _, s := range segs {
		if s == "node_modules" || s == ".git" {
			return -1
		}
	}
	parent := path.Dir(dir)
	switch parent {
	case "skills", "skills/.curated":
		return 1
	case ".claude/skills", ".codex/skills", ".agents/skills":
		return 2
	}
	if len(segs) <= 4 {
		return 3
	}
	return -1
}

// skills finds the skills in the repository at commit sha. When name is
// given, one whose folder is so named is read first, and the search stops
// at the first that is called name.
func (g github) skills(ctx context.Context, r resolved, name string) ([]Found, error) {
	var t struct {
		Tree []struct {
			Path, Type string
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := g.getJSON(ctx, "/repos/"+r.Repo+"/git/trees/"+r.commit+"?recursive=1", &t); err != nil {
		if errors.Is(err, errMissing) {
			return nil, missingRepo(r.Repo)
		}
		return nil, err
	}
	var cands []Found
	for _, e := range t.Tree {
		if e.Type != "blob" || path.Base(e.Path) != "SKILL.md" {
			continue
		}
		dir := path.Dir(e.Path)
		if dir == "." {
			dir = ""
		}
		if k := rank(dir); k >= 0 {
			cands = append(cands, Found{Path: dir, rank: k})
		}
	}
	if len(cands) == 0 && t.Truncated {
		return nil, proxy.Deny("github.com/%s is too large to search for skills: give the skill's folder, owner/repo/PATH", r.Repo)
	}
	slices.SortStableFunc(cands, func(a, b Found) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		return strings.Compare(a.Path, b.Path)
	})
	if len(cands) > maxCandidates {
		cands = cands[:maxCandidates]
	}
	if name != "" {
		for i, c := range cands {
			if path.Base(c.Path) == name || (c.Path == "" && path.Base(r.Repo) == name) {
				if g.frontmatter(ctx, r, &cands[i]); cands[i].Name == name {
					return []Found{cands[i]}, nil
				}
			}
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range cands {
		if cands[i].Name != "" {
			continue
		}
		wg.Add(1)
		go func(c *Found) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			g.frontmatter(ctx, r, c)
		}(&cands[i])
	}
	wg.Wait()
	out := cands[:0]
	for _, c := range cands {
		if c.Name != "" {
			out = append(out, c)
		}
	}
	return out, nil
}

// frontmatter fills in c's name and description from its SKILL.md; one
// that cannot be read leaves them empty.
func (g github) frontmatter(ctx context.Context, r resolved, c *Found) {
	res, err := g.open(ctx, g.raw+"/"+r.Repo+"/"+r.commit+"/"+escPath(path.Join(c.Path, "SKILL.md")))
	if err != nil {
		return
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	src := strings.ReplaceAll(string(b), "\r\n", "\n")
	rest, ok := strings.CutPrefix(src, "---\n")
	if !ok {
		return
	}
	front, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return
	}
	var f struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if yaml.Unmarshal([]byte(front), &f) != nil {
		return
	}
	c.Name, c.Description = strings.TrimSpace(f.Name), strings.Join(strings.Fields(f.Description), " ")
}

// pick is the skill called name among found, or why there is none.
func pick(r resolved, found []Found, name string) (Found, []string, error) {
	var hit []Found
	for _, f := range found {
		if f.Name == name {
			hit = append(hit, f)
		}
	}
	if len(hit) == 0 {
		where := r.Repo + " @ " + r.Ref
		if len(found) == 0 {
			return Found{}, nil, proxy.Deny("%s has no SKILL.md in its top folder, skills/, .claude/skills, .codex/skills or .agents/skills, nor 4 folders down: give the skill's folder, owner/repo/PATH", where)
		}
		names := make([]string, 0, len(found))
		for _, f := range found {
			names = append(names, f.Name)
		}
		slices.Sort(names)
		return Found{}, nil, proxy.Deny("%s has no skill named %s; it has: %s (add it by one of these names, or give its folder, owner/repo/PATH)", where, name, strings.Join(slices.Compact(names), ", "))
	}
	var warnings []string
	if len(hit) > 1 {
		warnings = append(warnings, fmt.Sprintf("%s has %d skills named %s; took the one in %s", r.Repo, len(hit), name, orTop(hit[0].Path)))
	}
	return hit[0], warnings, nil
}

func orTop(p string) string {
	if p == "" {
		return "its top folder"
	}
	return p
}

// files reads the files under p in the repository's tarball at r's
// commit. A link is refused, as it is in a folder of the Mac's.
func (g github) files(ctx context.Context, r resolved, p string) ([]File, error) {
	res, err := g.open(ctx, g.api+"/repos/"+r.Repo+"/tarball/"+r.commit)
	if err != nil {
		if errors.Is(err, errMissing) {
			return nil, missingRepo(r.Repo)
		}
		return nil, err
	}
	defer res.Body.Close()
	where := strings.TrimSuffix(r.Repo+"/"+p, "/") + " @ " + short(r.commit)
	gz, err := gzip.NewReader(io.LimitReader(res.Body, maxTarball))
	if err != nil {
		return nil, proxy.Deny("GitHub's tarball of %s could not be read: %v", r.Repo, err)
	}
	tr := tar.NewReader(io.LimitReader(gz, maxUnpacked))
	var (
		out   []File
		total int
		seen  bool
	)
	prefix := ""
	if p != "" {
		prefix = p + "/"
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, proxy.Deny("GitHub's tarball of %s could not be read (larger than %d MiB?): %v", r.Repo, maxTarball>>20, err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
			continue
		}
		// Every entry is under one folder, owner-repo-commit/.
		_, name, ok := strings.Cut(strings.TrimPrefix(h.Name, "./"), "/")
		if !ok {
			continue
		}
		name = strings.TrimSuffix(name, "/")
		if name == p || (p == "" && name == "") {
			seen = true
			continue
		}
		rel, ok := strings.CutPrefix(name, prefix)
		if !ok || rel == "" {
			continue
		}
		seen = true
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeSymlink, tar.TypeLink:
			return nil, proxy.Deny("%s/%s is a link: a skill is copied as it is, so the file itself must be there", where, rel)
		case tar.TypeReg, '\x00': // the old spelling of a file
		default:
			return nil, proxy.Deny("%s/%s is not a file or a folder", where, rel)
		}
		if h.Size > maxFileBytes {
			return nil, proxy.Deny("%s/%s is %d KiB; a skill's file may be up to %d KiB", where, rel, h.Size>>10, maxFileBytes>>10)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxFileBytes+1))
		if err != nil {
			return nil, proxy.Deny("GitHub's tarball of %s could not be read: %v", r.Repo, err)
		}
		total += len(b)
		if len(out) >= maxFiles || total > maxBytes {
			return nil, proxy.Deny("%s holds more than %d files or %d MiB", where, maxFiles, maxBytes>>20)
		}
		out = append(out, File{Path: rel, Exec: h.Mode&0o111 != 0, Content: b})
	}
	if !seen {
		return nil, proxy.Deny("%s has no folder %s at %s", r.Repo, p, short(r.commit))
	}
	return out, nil
}

// Changed is one file a newer commit changes in a skill's folder.
type Changed struct {
	Path                 string
	Status               string
	Additions, Deletions int
}

// compare is what changed in p between two commits.
func (g github) compare(ctx context.Context, repo, from, to, p string) ([]Changed, error) {
	var r struct {
		Files []struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		} `json:"files"`
	}
	if err := g.getJSON(ctx, "/repos/"+repo+"/compare/"+from+"..."+to, &r); err != nil {
		return nil, err
	}
	prefix := ""
	if p != "" {
		prefix = p + "/"
	}
	var out []Changed
	for _, f := range r.Files {
		if rel, ok := strings.CutPrefix(f.Filename, prefix); ok {
			out = append(out, Changed{Path: rel, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions})
		}
	}
	return out, nil
}

// fetch gets the skill called name from sp, checked, and where it came
// from; nothing is kept yet.
func (s Service) fetch(ctx context.Context, name string, sp Spec, ref string) (Skill, Origin, []string, error) {
	g := s.github()
	r, err := g.resolve(ctx, sp, ref)
	if err != nil {
		return Skill{}, Origin{}, nil, err
	}
	var warnings []string
	if !r.Exact {
		found, err := g.skills(ctx, r, name)
		if err != nil {
			return Skill{}, Origin{}, nil, err
		}
		f, w, err := pick(r, found, name)
		if err != nil {
			return Skill{}, Origin{}, nil, err
		}
		r.Path, warnings = f.Path, w
	}
	files, err := g.files(ctx, r, r.Path)
	if err != nil {
		return Skill{}, Origin{}, nil, err
	}
	o := Origin{Repo: r.Repo, Ref: r.Ref, Path: r.Path, Commit: r.commit, FetchedAt: time.Now().UTC().Truncate(time.Second)}
	sk, err := FromFiles(strings.TrimSuffix(r.Repo+"/"+r.Path, "/")+" @ "+short(r.commit), name, files)
	if err != nil {
		return sk, o, nil, err
	}
	if o.Tree, err = g.tree(ctx, r.Repo, r.commit, r.top, r.Path); err != nil {
		return sk, o, nil, err
	}
	o.Digest = digest(sk.Files)
	return sk, o, append(warnings, sk.Warnings...), nil
}

// digest is a skill's files, path, mode and content, as one hash.
func digest(files []File) string {
	fs := slices.Clone(files)
	slices.SortFunc(fs, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	h := sha256.New()
	for _, f := range fs {
		fmt.Fprintf(h, "%s\x00%t\x00%d\x00", f.Path, f.Exec, len(f.Content))
		h.Write(f.Content)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
