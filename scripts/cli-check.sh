#!/bin/sh
# Shows what moving the isolation image's Claude Code and Codex from the
# versions pinned in internal/isolation/image.go to newer ones changes of
# what gero's drivers parse, before an agent runs on them. It spends no
# tokens and touches no login: both CLIs run from a scratch directory, with
# a scratch home, only to print their help and protocol schema.
#   Codex: every method gero's driver names is still in the app-server
#     schema, the schema's diff, and the release notes in between.
#   Claude Code: every flag gero's driver passes is still in the binary
#     (some are hidden from --help), the --help diff, and the changelog in
#     between.
# The diffs stay in a directory it names at the end. What it cannot see,
# behaviour, is the live check in the README.
# Usage: sh scripts/cli-check.sh [CLAUDE_VERSION [CODEX_VERSION]]
#   (npm's latest by default; Apple silicon, like gero)
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# The diffs are kept here, and shown only when they are short.
keep=$(mktemp -d -t gero-cli-check)

die() {
    echo "cli-check: $*" >&2
    exit 1
}

# show FILE prints it, unless it is too long to read here.
show() {
    n=$(wc -l <"$1" | tr -d ' ')
    if [ "$n" -le 400 ]; then
        cat "$1"
    else
        echo "($n lines: $1)"
    fi
}

pinned() {
    sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\"\(.*\)\"/\1/p" "$root/internal/isolation/image.go"
}

claude_old=$(pinned claudeCodeVersion)
codex_old=$(pinned codexVersion)
[ -n "$claude_old" ] && [ -n "$codex_old" ] || die "no pinned versions in internal/isolation/image.go"
claude_new=${1:-$(npm view @anthropic-ai/claude-code version)}
codex_new=${2:-$(npm view @openai/codex version)}
echo "Claude Code $claude_old -> $claude_new, Codex $codex_old -> $codex_new"

# install PACKAGE VERSION puts the package in a directory of its own, its
# install scripts not run: nothing of it runs but what is asked below.
install() {
    d="$tmp/$(echo "$1" | tr '/@' '__')-$2"
    mkdir -p "$d"
    (cd "$d" && npm install --ignore-scripts --no-audit --no-fund --silent "$1@$2" >/dev/null) ||
        die "npm could not install $1@$2"
    echo "$d/node_modules"
}

broken=0

echo
echo "== Codex"
for v in "$codex_old" "$codex_new"; do
    m=$(install @openai/codex "$v")
    mkdir -p "$tmp/codex-home"
    CODEX_HOME="$tmp/codex-home" "$m/.bin/codex" app-server generate-json-schema --out "$tmp/schema-$v" ||
        die "codex $v did not write its schema"
done
# The methods and notifications the driver names (its imports left out).
for method in $(grep -v -E '^[[:space:]]*"[^"]*"$' "$root/internal/driver/codex.go" |
    grep -o '"[a-z][a-zA-Z]*/[a-zA-Z/]*"' | sort -u); do
    if ! grep -rqF -- "$method" "$tmp/schema-$codex_new"; then
        echo "MISSING: gero uses $method; codex $codex_new's schema has no such method"
        broken=1
    fi
done
echo "-- schema diff"
diff -u "$tmp/schema-$codex_old/codex_app_server_protocol.schemas.json" \
    "$tmp/schema-$codex_new/codex_app_server_protocol.schemas.json" | sed 1,2d >"$keep/codex-schema.diff" || true
show "$keep/codex-schema.diff"
echo "-- release notes (the pull requests left out)"
for tag in $(gh release list -R openai/codex --limit 100 --exclude-pre-releases --json tagName --jq '.[].tagName' |
    sed -n 's/^rust-v//p' | sort -V); do
    # Those after the pinned one, up to the new one.
    [ "$(printf '%s\n%s\n' "$tag" "$codex_old" | sort -V | tail -n 1)" = "$codex_old" ] && continue
    [ "$(printf '%s\n%s\n' "$tag" "$codex_new" | sort -V | tail -n 1)" = "$codex_new" ] || continue
    echo "# $tag"
    gh release view "rust-v$tag" -R openai/codex --json body --jq .body | sed '/^## Changelog/,$d'
done

echo
echo "== Claude Code"
for v in "$claude_old" "$claude_new"; do
    m=$(install @anthropic-ai/claude-code "$v")
    bin="$m/@anthropic-ai/claude-code-darwin-arm64/claude"
    [ -x "$bin" ] || die "claude $v has no darwin-arm64 binary"
    mkdir -p "$tmp/claude-home"
    [ "$(HOME="$tmp/claude-home" DISABLE_AUTOUPDATER=1 "$bin" --version)" = "$v (Claude Code)" ] ||
        die "the binary installed as claude $v does not say it is"
    HOME="$tmp/claude-home" DISABLE_AUTOUPDATER=1 "$bin" --help >"$tmp/help-$v.txt" 2>&1
    cp "$bin" "$tmp/claude-$v"
done
# The flags the driver passes, hidden ones (--resume-session-at) included.
for flag in $(grep -o '"--[a-z-]*"' "$root/internal/driver/claude.go" | tr -d '"' | sort -u); do
    if ! grep -qaF -e "$flag" "$tmp/claude-$claude_new"; then
        echo "MISSING: gero passes $flag; claude $claude_new has no such flag"
        broken=1
    fi
done
echo "-- --help diff"
diff -u "$tmp/help-$claude_old.txt" "$tmp/help-$claude_new.txt" | sed 1,2d >"$keep/claude-help.diff" || true
show "$keep/claude-help.diff"
echo "-- changelog (the IDE, cloud, Slack and review items left out)"
curl -fsSL -o "$tmp/CHANGELOG.md" https://raw.githubusercontent.com/anthropics/claude-code/refs/heads/main/CHANGELOG.md ||
    echo "(the changelog could not be fetched)"
awk -v new="## $claude_new" -v old="## $claude_old" '$0 == new { p = 1 } $0 == old { exit } p' "$tmp/CHANGELOG.md" 2>/dev/null |
    grep -v '^- \[' || echo "(no changelog entry for $claude_new)"

echo
echo "The diffs are in $keep."
if [ "$broken" = 1 ]; then
    echo "gero's drivers use something the new versions no longer have: see MISSING above"
    exit 1
fi
echo "Everything gero's drivers name is still there. Read the diffs and notes above, then run the live check (README)."
