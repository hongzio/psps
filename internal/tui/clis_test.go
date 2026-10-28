package tui

import (
	"strings"
	"testing"
)

func (h *harness) viewHas(wants ...string) {
	h.t.Helper()
	view := h.m.View()
	for _, want := range wants {
		if !strings.Contains(view, want) {
			h.t.Fatalf("missing %q:\n%s", want, view)
		}
	}
}

func (h *harness) cliCalls() string {
	h.be.mu.Lock()
	defer h.be.mu.Unlock()
	return strings.Join(h.be.cliCalls, "; ")
}

func TestClaudeAndCodexPageLogsClaudeInWithThePastedCode(t *testing.T) {
	h := newHarness(t)
	h.key("ctrl+g")
	h.key("l")
	h.viewHas("Accounts in dev", "2.1.311", "not logged in", "0.164.0", "✓ logged in", "ChatGPT")

	h.key("enter") // claude is under the cursor
	h.viewHas("Log claude in inside dev", "https://claude.com/cai/oauth/authorize?state=abc", "Paste it here")
	var copied string
	h.m.clipboard = func(s string) { copied = s }
	h.key("y")
	if copied != "https://claude.com/cai/oauth/authorize?state=abc" {
		t.Fatalf("copied %q", copied)
	}
	h.typeText("good#abc")
	if strings.Contains(h.m.View(), "good#abc") {
		t.Fatal("the code is shown")
	}
	h.key("enter")
	if got := h.cliCalls(); got != "start dev claude; code dev claude good#abc" {
		t.Fatalf("calls: %s", got)
	}
	if h.m.isoPage.clis.login != nil {
		t.Fatal("the finished login is still on screen")
	}
	h.viewHas("Claude subscription (max)", "claude is logged in in dev")
}

func TestClaudeAndCodexPageShowsCodexsCodeAndCancels(t *testing.T) {
	h := newHarness(t)
	h.key("ctrl+g")
	h.key("l")
	h.key("down")
	h.key("l")
	h.viewHas("Log codex in inside dev", "https://auth.openai.com/codex/device", "LK6M-R8WQK", "waiting for the browser")
	h.key("esc")
	if got := h.cliCalls(); got != "start dev codex; cancel dev codex" {
		t.Fatalf("calls: %s", got)
	}
	if h.m.isoPage.clis.login != nil || strings.Contains(h.m.View(), "LK6M-R8WQK") {
		t.Fatal("the cancelled login is still on screen")
	}

	h.key("o")
	h.key("y")
	if got := h.cliCalls(); !strings.HasSuffix(got, "logout dev codex") {
		t.Fatalf("calls: %s", got)
	}
	h.key("esc")
	if h.m.isoPage.clis != nil {
		t.Fatal("esc did not leave the page")
	}
}

func TestAccountsAreAddedAndUsedFromTheClaudeAndCodexPage(t *testing.T) {
	h := newHarness(t)
	h.key("ctrl+g")
	h.key("l")
	h.viewHas("Accounts", "none yet")

	// Choosing an account with none of the CLI's kind says how to add one.
	h.key("a")
	h.viewHas("no claude accounts yet")

	h.key("n")
	h.viewHas("New account", "claude-token: a token from `claude setup-token`")
	h.typeText("work")
	h.key("tab")
	h.key("tab")
	h.typeText("sk-ant-oat01-x")
	if strings.Contains(h.m.View(), "sk-ant-oat01-x") {
		t.Fatal("the secret is shown")
	}
	h.key("enter")
	h.viewHas("work", "claude-token", "unused")

	h.key("a")
	h.viewHas("What claude in dev uses")
	h.key("right")
	h.key("enter")
	h.viewHas("account work")
	if got := h.cliCalls(); got != "add work claude-token sk-ant-oat01-x; use dev claude work" {
		t.Fatalf("calls: %s", got)
	}

	// With an account in use, a login inside is not started under it.
	h.key("l")
	if strings.Contains(h.cliCalls(), "start") {
		t.Fatal("started a login the account would shadow")
	}

	h.key("down")
	h.key("down") // the account row
	h.key("s")
	h.typeText("sk-ant-oat01-y")
	h.key("enter")
	h.key("d")
	h.key("y")
	if got := h.cliCalls(); !strings.HasSuffix(got, "secret work sk-ant-oat01-y; rm work force=true") {
		t.Fatalf("calls: %s", got)
	}
}
