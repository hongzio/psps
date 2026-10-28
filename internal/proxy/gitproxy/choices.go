package gitproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/proxy"
)

// maxRepoPages bounds a listing at 1,000 repositories.
const maxRepoPages = 10

// Choices lists the repositories the env's credential opens, for the
// repos field: the token typed in the form or stored, or else what the
// token command prints. Each says whether the credential can push to it.
func (s Service) Choices(ctx context.Context, field string, env map[string]any, secret map[string]string) ([]proxy.Choice, error) {
	if field != "repos" {
		return nil, fmt.Errorf("git has no choices for %s", field)
	}
	token := strings.TrimSpace(secret["token"])
	if token == "" {
		cmd, err := commandOf(env["token_command"])
		if err != nil {
			return nil, err
		}
		if len(cmd) == 0 {
			return nil, proxy.Deny("enter a token first: the repositories listed are the ones it opens")
		}
		if token, err = NewTokenSource(cmd, 0).Get(); err != nil {
			return nil, err
		}
	}
	return listRepos(ctx, or(s.APIUpstream, "https://api.github.com"), token)
}

// commandOf is a token command as a form has it (a binary's name) or as
// it is stored (pinned argv).
func commandOf(v any) ([]string, error) {
	switch tc := v.(type) {
	case string:
		if strings.TrimSpace(tc) == "" {
			return nil, nil
		}
		return pinCommand(strings.TrimSpace(tc))
	case []any:
		var cmd []string
		for _, a := range tc {
			if s, ok := a.(string); ok {
				cmd = append(cmd, s)
			}
		}
		return cmd, nil
	case []string:
		return tc, nil
	}
	return nil, nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func listRepos(ctx context.Context, base, token string) ([]proxy.Choice, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	next := strings.TrimSuffix(base, "/") + "/user/repos?per_page=100&sort=full_name&affiliation=owner,collaborator,organization_member"
	var out []proxy.Choice
	for page := 0; next != "" && page < maxRepoPages; page++ {
		req, err := http.NewRequestWithContext(ctx, "GET", next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "gero-ghapi")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("could not reach %s: %w", base, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(body, &e) != nil || e.Message == "" {
				e.Message = fmt.Sprintf("%.200s", body)
			}
			return nil, proxy.Deny("GitHub refused the token (%d %s)", resp.StatusCode, e.Message)
		}
		var repos []struct {
			FullName    string `json:"full_name"`
			Private     bool   `json:"private"`
			Archived    bool   `json:"archived"`
			Permissions struct {
				Push bool `json:"push"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(body, &repos); err != nil {
			return nil, fmt.Errorf("unexpected answer from %s: %w", base, err)
		}
		for _, r := range repos {
			var notes []string
			if r.Private {
				notes = append(notes, "private")
			}
			if !r.Permissions.Push {
				notes = append(notes, "read-only")
			}
			if r.Archived {
				notes = append(notes, "archived")
			}
			out = append(out, proxy.Choice{Value: r.FullName, Note: strings.Join(notes, ", ")})
		}
		next = ""
		if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			next = m[1]
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Value) < strings.ToLower(out[j].Value) })
	return out, nil
}
