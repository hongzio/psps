package main

import (
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/paths"
)

// gero team help says how to use each command its usage names.
func TestTeamHelpNamesEveryTeamCommand(t *testing.T) {
	err := teamCmd(paths.Paths{}, nil)
	if err == nil {
		t.Fatal("no usage")
	}
	_, list, _ := strings.Cut(err.Error(), "usage: gero team ")
	list, _, _ = strings.Cut(list, " ")
	for _, c := range strings.Split(list, "|") {
		if !strings.Contains(teamHelp, "\n  gero team "+c+" ") {
			t.Errorf("gero team help says nothing of gero team %s", c)
		}
	}
}
