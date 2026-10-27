package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/api"
)

func TestVerifyExitsOnTheWorstIsolation(t *testing.T) {
	ok := api.Verify{Isolation: "a", Checks: []api.VerifyCheck{{Check: "Mac unreachable at 10.0.0.2", OK: true, Severity: api.SevIsolation, Got: "blocked"}}}
	setup := api.Verify{Isolation: "b", Checks: []api.VerifyCheck{{Check: "pg env prod answers from the Mac", Severity: api.SevConfig, Got: "refused", Detail: "no [pg prod] section"}}}
	broken := api.Verify{Isolation: "c", Checks: []api.VerifyCheck{{Check: "slack refuses a wrong workspace token", Severity: api.SevIsolation, Got: "200", Detail: "line one\nline two"}}}

	var out bytes.Buffer
	if code := renderVerify(&out, []api.Verify{ok, setup}, false); code != api.VerifyConfig {
		t.Errorf("ok+setup: %d", code)
	}
	if code := renderVerify(&out, []api.Verify{broken, setup, ok}, false); code != api.VerifyIsolation {
		t.Errorf("broken first: %d", code)
	}
	for _, want := range []string{"isolation c\n", "  FAIL slack refuses a wrong workspace token  200\n       line one\n       line two\n",
		"  WARN pg env prod answers from the Mac  refused\n       no [pg prod] section\n", "  1 checks passed.", "1/1 failed (1 isolation)."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := renderVerify(&out, []api.Verify{ok}, true); code != api.VerifyOK {
		t.Errorf("ok: %d", code)
	}
	var j struct {
		ExitCode   int `json:"exit_code"`
		Isolations []struct {
			Isolation string            `json:"isolation"`
			OK        bool              `json:"ok"`
			Checks    []api.VerifyCheck `json:"checks"`
		} `json:"isolations"`
	}
	if err := json.Unmarshal(out.Bytes(), &j); err != nil || j.ExitCode != 0 || len(j.Isolations) != 1 || !j.Isolations[0].OK || len(j.Isolations[0].Checks) != 1 {
		t.Errorf("json %v: %s", err, out.String())
	}
}
