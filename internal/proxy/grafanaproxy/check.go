package grafanaproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/creds"
)

// CheckResult is what each probe answered: a family's status (0 for the
// query family with no data source to probe) and each allowlisted folder's,
// read where saves are open.
type CheckResult struct {
	Families map[string]int
	Folders  map[string]int
}

var probes = map[string]func(Env) []string{
	"dashboards":  func(Env) []string { return []string{"/api/search?limit=1"} },
	"folders":     func(Env) []string { return []string{"/api/folders?limit=1"} },
	"datasources": func(Env) []string { return []string{"/api/datasources"} },
	"alerts":      func(Env) []string { return []string{"/api/v1/provisioning/alert-rules"} },
	"annotations": func(Env) []string { return []string{"/api/annotations?limit=1"} },
	"query": func(e Env) []string {
		var out []string
		for _, uid := range e.Datasources {
			out = append(out, "/api/datasources/uid/"+uid)
		}
		return out
	},
}

// CheckEnv asks Grafana, now, whether each allowed family actually opens,
// so a stale token, a Viewer role missing a permission or a wrong base URL
// is found by the operator at the terminal, not by the agent as an opaque
// 401. The query family is probed by reading each allowlisted data source's
// definition: it proves the token and the UIDs, though the datasources:query
// permission itself only shows at a real query. Saves are never probed (the
// only honest probe of a save is a dashboard that now exists), but where
// they are open each allowlisted folder is read.
func CheckEnv(ctx context.Context, rt http.RoundTripper, e Env, token string) (CheckResult, error) {
	up, err := UpstreamParts(e.BaseURL)
	if err != nil {
		return CheckResult{}, err
	}
	probe := func(path string) (int, error) {
		status, _, err := get(ctx, rt, up, token, path)
		if err != nil {
			return 0, fmt.Errorf("could not reach %s: %w", e.BaseURL, err)
		}
		return status, nil
	}
	status, err := probe("/api/health")
	if err != nil {
		return CheckResult{}, err
	}
	if status != 200 {
		return CheckResult{}, fmt.Errorf("%s is not answering as a Grafana instance (/api/health answered %d)", e.BaseURL, status)
	}
	res := CheckResult{Families: map[string]int{}, Folders: map[string]int{}}
	fams := append([]string(nil), e.Families...)
	sort.Strings(fams)
	for _, f := range fams {
		pf, ok := probes[f]
		if !ok {
			continue
		}
		res.Families[f] = 0
		for i, path := range pf(e) {
			s, err := probe(path)
			if err != nil {
				return res, err
			}
			if i == 0 || (res.Families[f] == 200 && s != 200) {
				res.Families[f] = s
			}
		}
	}
	if len(e.Writes) > 0 {
		for _, uid := range e.Folders {
			s, err := probe("/api/folders/" + uid)
			if err != nil {
				return res, err
			}
			res.Folders[uid] = s
		}
	}
	return res, nil
}

// Choice is a picker row: a UID and what a human recognises it by.
type Choice struct {
	UID, Desc string
}

// ListDatasources feeds a settings screen's picker and nothing in the
// boundary: the allowlist stays whatever the operator actually chose.
func ListDatasources(ctx context.Context, rt http.RoundTripper, e Env, token string) ([]Choice, error) {
	var items []struct{ UID, Name, Type string }
	if err := list(ctx, rt, e, token, "/api/datasources", &items); err != nil {
		return nil, err
	}
	var out []Choice
	for _, it := range items {
		if it.UID == "" {
			continue
		}
		desc := it.Name
		switch {
		case it.Name != "" && it.Type != "":
			desc = fmt.Sprintf("%s (%s)", it.Name, it.Type)
		case it.Name == "":
			desc = it.Type
		}
		out = append(out, Choice{it.UID, desc})
	}
	return out, nil
}

// ListFolders is every folder the token can see, for the same picker.
// Search rather than /api/folders, because the latter lists only the top
// level and a nested folder is as likely a sandbox as any.
func ListFolders(ctx context.Context, rt http.RoundTripper, e Env, token string) ([]Choice, error) {
	var items []struct {
		UID, Title  string
		FolderTitle string `json:"folderTitle"`
	}
	if err := list(ctx, rt, e, token, "/api/search?type=dash-folder&limit=5000", &items); err != nil {
		return nil, err
	}
	var out []Choice
	for _, it := range items {
		if it.UID == "" {
			continue
		}
		desc := it.Title
		if it.FolderTitle != "" {
			desc = it.FolderTitle + " / " + it.Title
		}
		out = append(out, Choice{it.UID, desc})
	}
	return out, nil
}

// list GETs a JSON array; a non-200 or a non-array answer is an empty one,
// since a picker with nothing in it is the honest rendering of either.
func list(ctx context.Context, rt http.RoundTripper, e Env, token, path string, into any) error {
	up, err := UpstreamParts(e.BaseURL)
	if err != nil {
		return err
	}
	status, data, err := get(ctx, rt, up, token, path)
	if err != nil {
		return err
	}
	if status == 200 {
		json.Unmarshal(data, into)
	}
	return nil
}

// Check probes the env from the Mac with its stored token.
func (s Service) Check(ctx context.Context, name string, cfg json.RawMessage, c *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	token, err := readToken(c, name)
	if err != nil {
		return "", err
	}
	res, err := CheckEnv(ctx, s.transport(), e, token)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "instance     %s — healthy\n", e.BaseURL)
	failures := 0
	fams := make([]string, 0, len(res.Families))
	for f := range res.Families {
		fams = append(fams, f)
	}
	sort.Strings(fams)
	for _, f := range fams {
		switch st := res.Families[f]; st {
		case 0:
			fmt.Fprintf(&b, "%-12s no data sources allowlisted; every query will be refused\n", f)
		case 200:
			fmt.Fprintf(&b, "%-12s open\n", f)
		default:
			fmt.Fprintf(&b, "%-12s answered %d — the service account likely lacks this read\n", f, st)
			failures++
		}
	}
	if len(fams) == 0 {
		b.WriteString("(no families allowed; only /api/health answers)\n")
	}
	if len(e.Writes) > 0 {
		uids := make([]string, 0, len(res.Folders))
		for u := range res.Folders {
			uids = append(uids, u)
		}
		sort.Strings(uids)
		for _, u := range uids {
			if st := res.Folders[u]; st == 200 {
				fmt.Fprintf(&b, "folder       %s visible — saves not probed; the service account needs Edit here\n", u)
			} else {
				fmt.Fprintf(&b, "folder       %s answered %d — missing, or the service account cannot see it\n", u, st)
				failures++
			}
		}
		if len(uids) == 0 {
			b.WriteString("folder       (none allowlisted; every dashboard save will be refused)\n")
		}
	}
	if failures > 0 {
		return b.String(), fmt.Errorf("%d probe(s) failed", failures)
	}
	return b.String(), nil
}
