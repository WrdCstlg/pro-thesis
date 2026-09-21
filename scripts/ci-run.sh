#!/usr/bin/env bash
# Run one `thesis` command in the current project directory, assert its exit
# code, and print what every oracle concluded in every world the command ran.
#
# A CI step that only checks an exit code cannot tell a real PASS from a world
# that drove nothing and was judged on nothing. The oracle explanations carry
# the evidence counts ("every one of the 8 key(s) admits a linearization
# (11452 operation(s) ...)"), so printing them makes a green step readable as
# a measurement.
#
# usage (from a project directory, e.g. testdata/kvfixture):
#   ../../scripts/ci-run.sh WANT_EXIT run --profile linear --fault '...'
set -u

want="$1"
shift
root="$(cd "$(dirname "$0")/.." && pwd)"

before="$(ls -1d .prothesis/runs/r_* 2>/dev/null | sort)"
"$root/bin/thesis" "$@"
got=$?
after="$(ls -1d .prothesis/runs/r_* 2>/dev/null | sort)"

new="$(comm -13 <(printf '%s\n' "$before") <(printf '%s\n' "$after") | grep -v '^$' | tail -n 1)"
if [ -n "$new" ]; then
  echo
  echo "--- oracle conclusions, run ${new##*/} ---"
  for f in "$new"/world-*/result.json; do
    [ -f "$f" ] || continue
    echo "${f#"$new"/}: outcome $(jq -r '.outcome' "$f")"
    jq -r '.oracles[] | "  \(.oracle): \(.status) - \(.explanation | .[0:220])"' "$f"
  done
else
  echo "no new run bundle was written"
fi

echo
echo "exit $got, want $want"
test "$got" -eq "$want"
