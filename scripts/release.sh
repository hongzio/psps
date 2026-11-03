#!/bin/sh
# Publishes psps <version>: tags main, builds psps for Apple silicon, attaches
# it to a GitHub release and points the formula in the Homebrew tap at it,
# from packaging/psps.rb. Homebrew installs that binary as it is.
# Usage: sh scripts/release.sh 0.1.0
#   TAP_DIR   hongzio/homebrew-tap clone (default: the one brew uses)
set -eu

version=${1:?usage: release.sh <version>}
root=$(cd "$(dirname "$0")/.." && pwd)
tap=${TAP_DIR:-$(brew --repository hongzio/tap)}
tag="v$version"
name="psps-$version-darwin-arm64"
out=$(mktemp -d)

die() {
    echo "release: $*" >&2
    exit 1
}

echo "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || die "version must look like 1.2.3, got '$version'"

cd "$root"
[ "$(git branch --show-current)" = main ] || die "not on main"
[ -z "$(git status --porcelain)" ] || die "uncommitted changes"
git fetch -q origin
git merge-base --is-ancestor origin/main main || die "main is behind origin/main"
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then die "tag $tag exists"; fi
[ -z "$(git -C "$tap" status --porcelain)" ] || die "uncommitted changes in $tap"
git -C "$tap" pull -q --ff-only
gh auth status >/dev/null 2>&1 || die "gh is not logged in"

mise run vet
mise run test </dev/null
# What the formula installs: the linux shim embedded, the version stamped.
mise run shim
GOOS=darwin GOARCH=arm64 mise exec -- go build -trimpath \
    -ldflags "-s -w -X github.com/hongzio/psps/internal/api.Version=$version" -o "$out/psps" ./cmd/psps
[ "$("$out/psps" version)" = "psps $version" ] || die "the build does not say it is $version"
lipo "$out/psps" -verify_arch arm64 || die "the build is not arm64"
tar -C "$out" -czf "$out/$name.tar.gz" psps
sha=$(shasum -a 256 "$out/$name.tar.gz" | cut -d ' ' -f 1)

git tag -a "$tag" -m "psps $version"
git push -q origin main "$tag"
gh release create "$tag" "$out/$name.tar.gz" --title "psps $version" --generate-notes

mkdir -p "$tap/Formula"
sed -e "s/@VERSION@/$version/g" -e "s/@SHA256@/$sha/" packaging/psps.rb >"$tap/Formula/psps.rb"
git -C "$tap" add Formula/psps.rb
git -C "$tap" commit -q -m "psps $version"
git -C "$tap" push -q
echo "released psps $version"
