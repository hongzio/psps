package main

import (
	"slices"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/store"
)

func exposed(envs []string, missing ...string) api.Exposure {
	return api.Exposure{Service: "skill", Enabled: true, Exposure: store.Exposure{Envs: envs, Default: envs[0]}, Missing: missing}
}

func TestExposeEnvsAddsToTheList(t *testing.T) {
	envs, change := exposeEnvs(exposed([]string{"ship", "research"}), []string{"wrap-up", "ship"}, false)
	if want := []string{"ship", "research", "wrap-up"}; !slices.Equal(envs, want) {
		t.Fatalf("envs = %v, want %v", envs, want)
	}
	if change != "added wrap-up" {
		t.Fatalf("change = %q", change)
	}
}

func TestExposeEnvsOnlyReplacesTheList(t *testing.T) {
	envs, change := exposeEnvs(exposed([]string{"ship", "research"}), []string{"wrap-up"}, true)
	if want := []string{"wrap-up"}; !slices.Equal(envs, want) {
		t.Fatalf("envs = %v, want %v", envs, want)
	}
	if change != "added wrap-up, removed ship,research" {
		t.Fatalf("change = %q", change)
	}
}

func TestExposeEnvsDropsEnvsNoLongerDefined(t *testing.T) {
	envs, change := exposeEnvs(exposed([]string{"ship", "gone"}, "gone"), []string{"wrap-up"}, false)
	if want := []string{"ship", "wrap-up"}; !slices.Equal(envs, want) {
		t.Fatalf("envs = %v, want %v", envs, want)
	}
	if change != "added wrap-up, removed gone" {
		t.Fatalf("change = %q", change)
	}
}

func TestExposeEnvsWhenOff(t *testing.T) {
	envs, change := exposeEnvs(api.Exposure{Service: "skill"}, []string{"a", "b"}, false)
	if want := []string{"a", "b"}; !slices.Equal(envs, want) || change != "added a,b" {
		t.Fatalf("envs = %v change = %q", envs, change)
	}
}

func TestExposeEnvsNothingNew(t *testing.T) {
	_, change := exposeEnvs(exposed([]string{"ship"}), []string{"ship"}, false)
	if change != "no env added" {
		t.Fatalf("change = %q", change)
	}
}

func TestUnexposeEnvsTakesOnlyThoseOut(t *testing.T) {
	envs, change, err := unexposeEnvs(exposed([]string{"ship", "research", "wrap-up"}), []string{"research"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ship", "wrap-up"}; !slices.Equal(envs, want) || change != "removed research" {
		t.Fatalf("envs = %v change = %q", envs, change)
	}
	envs, _, err = unexposeEnvs(exposed([]string{"ship"}), []string{"ship"})
	if err != nil || envs == nil || len(envs) != 0 {
		t.Fatalf("last env out: envs = %#v err = %v (want an empty, non-nil list)", envs, err)
	}
}

func TestUnexposeEnvsRefusesOneNotExposed(t *testing.T) {
	_, _, err := unexposeEnvs(exposed([]string{"ship"}), []string{"ship", "nope"})
	if err == nil || err.Error() != "nope not exposed (exposed: ship)" {
		t.Fatalf("err = %v", err)
	}
}

func TestSplitEnvs(t *testing.T) {
	if got := splitEnvs(" a, b,,a "); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
}
