#!/usr/bin/env bash
#
# Render `govulncheck -format json` output as a collapsed Markdown report,
# used by CI for the PR comment and the job summary.
#
# JSON mode always exits 0, so this script restores text mode's contract:
# exit 3 when a vulnerable symbol is reachable from our code. Vulnerabilities
# found only at module or package level (imported, never called) are counted
# in the summary line but do not fail.
#
# Usage:
#   ./hack/govulncheck-report.sh govulncheck.json > report.md

set -euo pipefail

input=${1:?usage: $0 <govulncheck.json>}

IFS=$' \t\r' read -r reachable unreached < <(jq -rs '
  ([.[] | .finding? // empty | .osv] | unique) as $all
  | ([.[] | .finding? // empty | select(.trace[0].function) | .osv] | unique) as $called
  | "\($called | length) \(($all - $called) | length)"
' "$input")

if ((reachable == 0)); then
	if ((unreached == 0)); then
		echo "✅ **govulncheck**: no known vulnerabilities."
	else
		echo "✅ **govulncheck**: no vulnerabilities reachable from our code ($unreached in imported modules, never called)."
	fi
	exit 0
fi

jq -rs --argjson unreached "$unreached" '
  def plural($n; $word): "\($n) \($word)\(if $n == 1 then "y" else "ies" end)";
  def version($module): if $module == "stdlib" then "go" + ltrimstr("v") else . end;
  def cell: gsub("\\|"; "\\|") | gsub("\n"; " ");

  ([.[] | .osv? // empty | {key: .id, value: .summary}] | from_entries) as $summaries
  | [.[] | .finding? // empty | select(.trace[0].function)] as $called
  | ($called[0].trace[-1].module) as $main
  | ($called | group_by(.osv)) as $groups
  | "<details>",
    "<summary>⚠️ <b>govulncheck</b>: \(plural($groups | length; "vulnerabilit")) reachable from our code"
      + (if $unreached > 0 then " (\($unreached) more in imported modules, never called)" else "" end)
      + "</summary>",
    "",
    "| ID | Module | Found | Fixed | Reached from | Summary |",
    "|----|--------|-------|-------|--------------|---------|",
    ($groups[]
      | .[0] as $f
      | ($f.trace[0]) as $vuln
      | (first($f.trace[] | select(.module == $main)).position) as $site
      | "| [\($f.osv)](https://pkg.go.dev/vuln/\($f.osv))"
        + " | `\($vuln.module)`"
        + " | \($vuln.version | version($vuln.module))"
        + " | \(($f.fixed_version // "") | if . == "" then "none" else version($vuln.module) end)"
        + " | `\($site.filename):\($site.line)`"
        + " | \($summaries[$f.osv] // "" | cell) |"),
    "",
    "</details>"
' "$input"

exit 3
