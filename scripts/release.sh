#!/bin/sh
# Publishes gero <version>: tags main, makes a GitHub release of the tag and
# points the formula in the Homebrew tap at its source archive, from
# packaging/gero.rb. Homebrew builds gero from that archive on install.
# Usage: sh scripts/release.sh 0.1.0
#   TAP_DIR   hongzio/homebrew-tap clone (default: the one brew uses)
set -eu

version=${1:?usage: release.sh <version>}
root=$(cd "$(dirname "$0")/.." && pwd)
tap=${TAP_DIR:-$(brew --repository hongzio/tap)}
tag="v$version"
archive="https://github.com/hongzio/gero/archive/refs/tags/$tag.tar.gz"

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
mise run build

git tag -a "$tag" -m "gero $version"
git push -q origin main "$tag"
gh release create "$tag" --title "gero $version" --generate-notes
sha=$(curl -fsSL "$archive" | shasum -a 256 | cut -d ' ' -f 1)

mkdir -p "$tap/Formula"
sed -e "s/@VERSION@/$version/" -e "s/@SHA256@/$sha/" packaging/gero.rb >"$tap/Formula/gero.rb"
git -C "$tap" add Formula/gero.rb
git -C "$tap" commit -q -m "gero $version"
git -C "$tap" push -q
echo "released gero $version"
