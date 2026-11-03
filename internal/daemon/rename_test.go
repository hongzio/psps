package daemon

import (
	"context"
	"slices"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/isolation"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/shimproto"
)

func TestAShimFromBeforeTheRenameKeepsItsFilesUnderGero(t *testing.T) {
	req := shimproto.SyncRequest{
		Files:  []shimproto.SyncFile{{Path: "/home/agent/.claude/skills/psps-mail/SKILL.md"}, {Path: "/etc/psps/env.sh"}, {Path: "/home/agent/x/psps-y"}},
		Remove: []string{"/home/agent/.codex/skills/psps-cron", "/etc/psps/pgpass"},
		Prune:  []string{"/home/agent/.claude/skills/psps-skill-"},
	}
	got := asGero(req)
	if got.Files[0].Path != "/home/agent/.claude/skills/gero-mail/SKILL.md" || got.Files[1].Path != "/etc/gero/env.sh" || got.Files[2].Path != "/home/agent/x/psps-y" ||
		!slices.Equal(got.Remove, []string{"/home/agent/.codex/skills/gero-cron", "/etc/gero/pgpass"}) ||
		!slices.Equal(got.Prune, []string{"/home/agent/.claude/skills/gero-skill-"}) {
		t.Errorf("%+v", got)
	}
	if req.Files[0].Path != "/home/agent/.claude/skills/psps-mail/SKILL.md" || req.Remove[0] != "/home/agent/.codex/skills/psps-cron" {
		t.Errorf("the request itself changed: %+v", req)
	}
}

// One installing psps meets no gero: a new isolation is named psps's way,
// inside too, and its agents have no GERO_ variables. One from gero's time
// keeps its names.
func TestANewIsolationIsPspssAndOneFromGerosTimeKeepsItsNames(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	iso, err := m.CreateIsolation(context.Background(), api.CreateIsolation{Name: "fresh"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := iso.Container
	if c != "psps-"+iso.ID || iso.Network != c+"-net" || isolation.HomeVolume(c) != c+"-home" ||
		isolation.StateRoot(c) != "/var/lib/psps" || isolation.AgentStateDir(c, "a") != "/var/lib/psps/agents/a" {
		t.Errorf("new: %+v, state root %s", iso, isolation.StateRoot(c))
	}
	if m.fromGero(iso.ID) || len(proxy.AgentEnv("MAIL_URL", "u", m.fromGero(iso.ID))) != 1 {
		t.Error("a new isolation's agents get GERO_ variables")
	}
	if !m.fromGero(testIsoID) || isolation.StateRoot(testCtr) != "/var/lib/gero" || isolation.HomeVolume(testCtr) != "gero-"+testIsoID+"-home" {
		t.Errorf("one from gero's time: %s", isolation.StateRoot(testCtr))
	}
	if env := proxy.AgentEnv("MAIL_URL", "u", true); !slices.Equal(env, []string{"PSPS_MAIL_URL=u", "GERO_MAIL_URL=u"}) {
		t.Errorf("%q", env)
	}
}
