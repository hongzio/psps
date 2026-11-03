package daemon

import (
	"slices"
	"testing"

	"github.com/hongzio/psps/internal/shimproto"
)

func TestAShimFromBeforeTheRenameKeepsItsSkillsUnderGero(t *testing.T) {
	req := shimproto.SyncRequest{
		Files:  []shimproto.SyncFile{{Path: "/home/agent/.claude/skills/psps-mail/SKILL.md"}, {Path: "/etc/gero/env.sh"}},
		Remove: []string{"/home/agent/.codex/skills/psps-cron"},
		Prune:  []string{"/home/agent/.claude/skills/psps-skill-"},
	}
	got := asGero(req)
	if got.Files[0].Path != "/home/agent/.claude/skills/gero-mail/SKILL.md" || got.Files[1].Path != "/etc/gero/env.sh" ||
		!slices.Equal(got.Remove, []string{"/home/agent/.codex/skills/gero-cron"}) ||
		!slices.Equal(got.Prune, []string{"/home/agent/.claude/skills/gero-skill-"}) {
		t.Errorf("%+v", got)
	}
	if req.Files[0].Path != "/home/agent/.claude/skills/psps-mail/SKILL.md" || req.Remove[0] != "/home/agent/.codex/skills/psps-cron" {
		t.Errorf("the request itself changed: %+v", req)
	}
}
