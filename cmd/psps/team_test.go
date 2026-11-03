package main

import (
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/paths"
)

// psps team help says how to use each command its usage names.
func TestTeamHelpNamesEveryTeamCommand(t *testing.T) {
	err := teamCmd(paths.Paths{}, nil)
	if err == nil {
		t.Fatal("no usage")
	}
	_, list, _ := strings.Cut(err.Error(), "usage: psps team ")
	list, _, _ = strings.Cut(list, " ")
	for _, c := range strings.Split(list, "|") {
		if !strings.Contains(teamHelp, "\n  psps team "+c+" ") {
			t.Errorf("psps team help says nothing of psps team %s", c)
		}
	}
}
