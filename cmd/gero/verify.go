package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hongzio/gero/internal/api"
)

// renderVerify prints `gero iso verify` the way isolate does: a line per
// check, the reason under each failure, a count at the end. FAIL is a
// broken guarantee, WARN something set up wrong. It returns the exit code,
// the worst over every isolation.
func renderVerify(w io.Writer, reports []api.Verify, asJSON bool) int {
	code := api.VerifyOK
	for _, v := range reports {
		if c := v.ExitCode(); c == api.VerifyIsolation || (c == api.VerifyConfig && code == api.VerifyOK) {
			code = c
		}
	}
	if asJSON {
		type report struct {
			api.Verify
			OK       bool `json:"ok"`
			ExitCode int  `json:"exit_code"`
		}
		out := struct {
			ExitCode   int      `json:"exit_code"`
			Isolations []report `json:"isolations"`
		}{ExitCode: code}
		for _, v := range reports {
			out.Isolations = append(out.Isolations, report{Verify: v, OK: v.ExitCode() == api.VerifyOK, ExitCode: v.ExitCode()})
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(w, string(b))
		return code
	}
	for i, v := range reports {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "isolation %s\n", v.Isolation)
		width := 0
		for _, c := range v.Checks {
			width = max(width, len(c.Check))
		}
		failed, broken := 0, 0
		for _, c := range v.Checks {
			fmt.Fprintf(w, "  %-4s %-*s  %s\n", verifyMark(c), width, c.Check, c.Got)
			if !c.OK {
				failed++
				if c.Severity == api.SevIsolation {
					broken++
				}
				if c.Detail != "" {
					fmt.Fprintf(w, "       %s\n", strings.ReplaceAll(c.Detail, "\n", "\n       "))
				}
			}
		}
		fmt.Fprintln(w)
		if failed == 0 {
			fmt.Fprintf(w, "  %d checks passed.\n", len(v.Checks))
		} else {
			fmt.Fprintf(w, "  %d/%d failed (%d isolation).\n", failed, len(v.Checks), broken)
		}
	}
	return code
}

func verifyMark(c api.VerifyCheck) string {
	switch {
	case c.OK:
		return "PASS"
	case c.Severity == api.SevIsolation:
		return "FAIL"
	}
	return "WARN"
}
