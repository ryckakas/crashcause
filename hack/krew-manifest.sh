#!/usr/bin/env bash
#
# Rewrite the krew manifest's placeholders from a release's checksums.
#
# deploy/krew/crashcause.yaml is committed in PLACEHOLDER form: version
# v0.0.0 and all-zero sha256 digests that can never pass krew's checksum
# check. This script produces the installable form for one release: it
# stamps the tag into spec.version and every archive uri, and fills in each
# platform's sha256 from the release's checksums.txt -- pairing digest to
# platform BY ARCHIVE FILENAME, never by list order, so a reordered platform
# list cannot pair a digest with the wrong platform.
#
# Every sed pattern is anchored to its YAML indentation on purpose: the
# unanchored forms also match the comment block at the top of the manifest,
# and rewriting the documentation along with the data is how the previous
# manual recipe once mangled itself.
#
# The release workflow's krew job runs this against the goreleaser-generated
# checksums and uploads the result as the release's `crashcause.yaml` asset;
# the repo copy is never rewritten.
#
# Usage: ./hack/krew-manifest.sh <tag> <checksums.txt> [manifest]
#        ./hack/krew-manifest.sh v0.1.2 crashcause_0.1.2_checksums.txt

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[ $# -ge 2 ] || fail "usage: $0 <tag> <checksums.txt> [manifest]"
TAG="$1"
CHECKSUMS="$2"
MANIFEST="${3:-${REPO_ROOT}/deploy/krew/crashcause.yaml}"

echo "${TAG}" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || fail "tag '${TAG}' is not vX.Y.Z"
VERSION="${TAG#v}"
[ -f "${CHECKSUMS}" ] || fail "checksums file not found: ${CHECKSUMS}"
[ -f "${MANIFEST}" ] || fail "manifest not found: ${MANIFEST}"

grep -q '^  version: v0\.0\.0$' "${MANIFEST}" \
  || fail "manifest is not in placeholder form (expected '  version: v0.0.0'); start from the committed copy"

# Stamp the tag into spec.version and the archive uris.
sed -i \
  -e "s|^  version: v0\.0\.0$|  version: ${TAG}|" \
  -e "s|^\(      uri: .*\)/download/v0\.0\.0/|\1/download/${TAG}/|" \
  -e "s|^\(      uri: .*\)crashcause_0\.0\.0_|\1crashcause_${VERSION}_|" \
  "${MANIFEST}"

# Fill each platform's sha256 from the checksums line for the archive named
# on the immediately preceding uri line (checksums.txt format:
# "<64-hex-digest>  <filename>").
awk -v checksums="${CHECKSUMS}" '
  BEGIN {
    while ((getline line < checksums) > 0) {
      n = split(line, parts, /[[:space:]]+/)
      if (n >= 2) digest[parts[2]] = parts[1]
    }
    close(checksums)
  }
  /^      uri: / {
    n = split($0, seg, "/")
    fname = seg[n]
  }
  /^      sha256: / {
    if (fname == "") {
      print "FAIL: sha256 line with no preceding uri line" > "/dev/stderr"
      exit 1
    }
    if (!(fname in digest)) {
      print "FAIL: no checksum found for " fname > "/dev/stderr"
      exit 1
    }
    if (digest[fname] !~ /^[0-9a-f]{64}$/) {
      print "FAIL: checksum for " fname " is not 64 hex chars" > "/dev/stderr"
      exit 1
    }
    print "      sha256: \"" digest[fname] "\""
    fname = ""
    next
  }
  { print }
' "${MANIFEST}" > "${MANIFEST}.tmp"
mv "${MANIFEST}.tmp" "${MANIFEST}"

# Post-conditions: fail loudly rather than upload a half-rewritten manifest.
grep -q "^  version: ${TAG}$" "${MANIFEST}" || fail "spec.version was not stamped to ${TAG}"
if grep -v '^#' "${MANIFEST}" | grep -q '0\.0\.0'; then
  fail "a 0.0.0 placeholder survived outside the comment block"
fi
if grep -q 'sha256: "0\{64\}"' "${MANIFEST}"; then
  fail "an all-zero sha256 placeholder survived"
fi
FILLED="$(grep -cE '^      sha256: "[0-9a-f]{64}"$' "${MANIFEST}")"
[ "${FILLED}" -eq 4 ] || fail "expected exactly 4 filled sha256 lines, found ${FILLED}"

echo "OK: ${MANIFEST} rewritten for ${TAG} (4 platform digests filled)"
