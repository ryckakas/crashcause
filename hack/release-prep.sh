#!/usr/bin/env bash
#
# Cut the CHANGELOG's Unreleased section as a release.
#
# Given VERSION (X.Y.Z, no leading v), this inserts a "## [X.Y.Z] - <date>"
# heading directly under "## [Unreleased]" -- the accumulated Unreleased
# entries become the release's entries and Unreleased itself is left empty --
# and rewrites the link-reference block at the bottom so [Unreleased]
# compares from the new tag and [X.Y.Z] compares against the previous
# release.
#
# It refuses to run when the version is not strict X.Y.Z, when tag vX.Y.Z or
# a [X.Y.Z] section already exists, or when the Unreleased section has no
# entries: an empty release usually means the wrong branch or an
# already-cut CHANGELOG is checked out.
#
# .github/workflows/release-prep.yml runs this and opens the release PR; it
# works just as well from a local checkout (GNU sed required - the in-place
# and append forms used here break on macOS/BSD sed).
#
# Usage: ./hack/release-prep.sh <version>     # e.g. ./hack/release-prep.sh 0.1.2

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
# Overridable for tests, so a dry run never mutates the real changelog.
CHANGELOG="${CHANGELOG:-${REPO_ROOT}/CHANGELOG.md}"
REPO_URL="https://github.com/ryckakas/crashcause"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[ $# -eq 1 ] || fail "usage: $0 <version>  (X.Y.Z, no leading v)"
VERSION="$1"

echo "${VERSION}" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' \
  || fail "version '${VERSION}' is not X.Y.Z (no leading v, no pre-release suffix)"
TAG="v${VERSION}"

if git -C "${REPO_ROOT}" rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
  fail "tag ${TAG} already exists"
fi

[ -f "${CHANGELOG}" ] || fail "changelog not found at ${CHANGELOG}"
grep -q '^## \[Unreleased\]$' "${CHANGELOG}" \
  || fail "no '## [Unreleased]' heading in ${CHANGELOG}"
grep -q '^\[Unreleased\]: ' "${CHANGELOG}" \
  || fail "no '[Unreleased]: ' link reference at the bottom of ${CHANGELOG}"
if grep -q "^## \[${VERSION}\]" "${CHANGELOG}"; then
  fail "the CHANGELOG already has a [${VERSION}] section"
fi

# The Unreleased section must contain at least one non-blank line before the
# next "## [" heading, otherwise the release notes would be empty.
entries="$(awk '/^## \[Unreleased\]$/{in_section=1; next} /^## \[/{in_section=0} in_section' "${CHANGELOG}" \
  | grep -c -v '^[[:space:]]*$' || true)"
[ "${entries}" -gt 0 ] || fail "the [Unreleased] section is empty - nothing to release"

# The previous release is the newest existing "## [X.Y.Z]" heading (the file
# is reverse-chronological); the new compare link is anchored against it.
PREV="$(grep -oE '^## \[[0-9]+\.[0-9]+\.[0-9]+\]' "${CHANGELOG}" | head -1 | tr -d '#[] ')"
[ -n "${PREV}" ] || fail "could not find a previous release heading to link against"

TODAY="$(date -u +%Y-%m-%d)"

sed -i \
  -e "s|^## \[Unreleased\]$|## [Unreleased]\n\n## [${VERSION}] - ${TODAY}|" \
  -e "s|^\[Unreleased\]: .*|[Unreleased]: ${REPO_URL}/compare/${TAG}...HEAD|" \
  "${CHANGELOG}"
sed -i "/^\[Unreleased\]: /a\\
[${VERSION}]: ${REPO_URL}/compare/v${PREV}...${TAG}" "${CHANGELOG}"

echo "OK: cut [Unreleased] as [${VERSION}] - ${TODAY} (previous release: ${PREV})"
