package main

import (
	"reflect"
	"testing"

	"github.com/hongzio/gero/internal/proxy/gitproxy"
)

func TestFlagsBecomeAPatchOfOnlyWhatWasGiven(t *testing.T) {
	fields := gitproxy.Service{}.Fields()
	p, pos, err := parsePatch("t", fields, []string{"work", "--allow-repo", "a/b", "--allow-repo", "c/d,e/f", "--read-write", "--clear-api-writes"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pos, []string{"work"}) {
		t.Fatalf("positional = %v", pos)
	}
	want := map[string]any{"repos": []string{"a/b", "c/d", "e/f"}, "read_only": false}
	if !reflect.DeepEqual(p.Set, want) || !reflect.DeepEqual(p.Clear, []string{"api_writes"}) {
		t.Fatalf("patch = %+v", p)
	}
	// An edit that names nothing changes nothing.
	if p, _, _ := parsePatch("t", fields, []string{"work"}); len(p.Set) != 0 || len(p.Clear) != 0 {
		t.Fatalf("empty edit = %+v", p)
	}
	if _, _, err := parsePatch("t", fields, []string{"work", "--read-only", "--read-write"}); err == nil {
		t.Fatal("contradicting flags accepted")
	}
}
