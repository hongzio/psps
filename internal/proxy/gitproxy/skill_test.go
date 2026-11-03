package gitproxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/store"
)

// Past ten names the skill says how many and where they are listed, while
// the API side is on to list them.
func TestALongAllowlistPointsAtTheListing(t *testing.T) {
	var repos []string
	for i := range 11 {
		repos = append(repos, fmt.Sprintf("org/r%d", i))
	}
	v := proxy.View{Exposure: store.Exposure{Envs: []string{"axis"}}}
	envs := map[string]Env{"axis": {Repos: repos}}
	if s := skillBody(v, envs, Settings{API: true}); !strings.Contains(s, "11 repositories; `GET $GITHUB_API_URL/user/repos` lists them") ||
		!strings.Contains(s, `curl -sS "$GITHUB_API_URL/user/repos"`) {
		t.Fatal(s)
	}
	if s := skillBody(v, envs, Settings{}); strings.Contains(s, "/user/repos") || !strings.Contains(s, "11 repositories; try the clone") {
		t.Fatal(s)
	}
}
