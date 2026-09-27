#!/usr/bin/env bash
# check-portability.sh -- lint ratchet for the "bash 3.2 + macOS/Linux"
# constraint this repo runs under (macOS ships bash 3.2 at /bin/bash; several
# scripts here are also used on Linux CI/lab hosts). Turns that constraint
# into a runnable gate instead of a comment people have to remember.
#
# What it checks, on every tracked *.sh file:
#   1. `bash -n` parses cleanly                                (hard FAIL)
#   2. shellcheck (if on PATH): report warning/error counts;
#      only *error* severity is a hard FAIL (repo baseline: 0)  (hard FAIL)
#   3. banned bash-4/GNU-only constructs are absent:
#        declare -A / local -A   (associative arrays -- bash 4+)
#        mapfile / readarray     (bash 4+ builtins)
#        ${var^^} ${var,,} ${var^} ${var,}   (bash 4+ case-fold expansion)
#      `declare -A`/`local -A` are tolerated ONLY in the four files listed
#      in ALLOWLIST_DECLARE_A below (known, human-gated sites tracked in
#      issue #105 -- deferred/safety-critical logic, out of scope for the
#      rest of this repo). mapfile/readarray and case-fold expansions are
#      NEVER allowlisted, anywhere, including in those four files.
#   4. GNU-only tool flags that differ or are absent on macOS/BSD are
#      reported as WARNings (informational, non-fatal): grep -P, sed -i
#      (any form), readlink -f, date -d, mapfile. (readlink -f legitimately
#      appears in Linux-only /proc code elsewhere in this repo, hence
#      warn-not-fail rather than ban.)
#
# Exit 0 = green (no hard FAILs). Exit 1 = at least one hard FAIL.
#
# Usage: ./check-portability.sh

cd "$(dirname "$0")" || exit 1

SELF_NAME="check-portability.sh"

# ---- 1. enumerate shell scripts -------------------------------------------
# --cached AND --others (with --exclude-standard, so .gitignore still wins):
# a new script is untracked until it is `git add`ed, and that is exactly when
# this check earns its keep. Tracked-only enumeration meant an author could
# write new.sh using `declare -A`, run this, get PASS because git ls-files
# never listed the file, and commit the breakage -- the ratchet was blind at
# the one moment it had to bite.
FILES=""
if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  FILES="$(git ls-files --cached --others --exclude-standard '*.sh' 2>/dev/null | sort -u)"
fi
if [ -z "$FILES" ]; then
  # Fallback for a non-git checkout: walk the tree, skip .git.
  FILES="$(find . -name '.git' -prune -o -type f -name '*.sh' -print | sed 's#^\./##')"
fi

if [ -z "$FILES" ]; then
  echo "check-portability.sh: no *.sh files found under $(pwd) -- nothing to check" >&2
  exit 0
fi

# ---- known, human-gated declare -A / local -A sites (issue #105) ---------
# Per-file allowlist (not per-line: simpler, and these files are wholesale
# out of scope per the same issue). Do NOT add to this list without a
# matching issue/PR discussion -- it is meant to stay exactly these four.
ALLOWLIST_DECLARE_A=(
  "netlab/fleet.sh"
  "lab/oracle.sh"
  "netlab/health.sh"
  "lab/scenarios/13-topo-all-clients-soak.sh"
)

_is_allowlisted() {
  local f="$1" a
  for a in "${ALLOWLIST_DECLARE_A[@]}"; do
    [ "$f" = "$a" ] && return 0
  done
  return 1
}

_is_self() {
  case "$1" in
    "$SELF_NAME"|*"/$SELF_NAME") return 0 ;;
    *) return 1 ;;
  esac
}

n_scanned=0
n_parse_ok=0
n_parse_fail=0
parse_fail_report=""

n_banned_fail=0
banned_fail_report=""
n_allowlisted=0
allowlisted_report=""

n_warn=0
warn_report=""

# Iterate by newline (not word-splitting) so filenames with spaces survive;
# a plain for-loop (not a `... | while read` pipe) so counters set inside
# the loop are still visible after it, on bash 3.2 and Linux bash alike.
OLDIFS="$IFS"
IFS='
'
for f in $FILES; do
  IFS="$OLDIFS"
  [ -n "$f" ] || continue
  [ -f "$f" ] || continue
  n_scanned=$((n_scanned + 1))

  # ---- 2. bash -n parse check ----
  if err=$(bash -n "$f" 2>&1 >/dev/null); then
    n_parse_ok=$((n_parse_ok + 1))
  else
    n_parse_fail=$((n_parse_fail + 1))
    parse_fail_report="${parse_fail_report}  FAIL ${f}: ${err}
"
  fi

  # ---- 3./4. grep-based checks (skip self: our own pattern strings and
  # this comment block would otherwise self-match) ----
  if ! _is_self "$f"; then
    # declare -A / local -A -- allowlisted by filename (issue #105)
    while IFS= read -r hit; do
      [ -n "$hit" ] || continue
      if _is_allowlisted "$f"; then
        n_allowlisted=$((n_allowlisted + 1))
        allowlisted_report="${allowlisted_report}  ALLOWLISTED ${f}:${hit}
"
      else
        n_banned_fail=$((n_banned_fail + 1))
        banned_fail_report="${banned_fail_report}  FAIL ${f}:${hit} (declare -A / local -A -- bash 4+, not allowlisted)
"
      fi
    done <<EOF
$(grep -nE '(^|[^A-Za-z0-9_])(declare|local)[[:space:]]+-A' "$f" | cut -d: -f1)
EOF

    # mapfile / readarray -- never allowlisted
    while IFS= read -r hit; do
      [ -n "$hit" ] || continue
      n_banned_fail=$((n_banned_fail + 1))
      banned_fail_report="${banned_fail_report}  FAIL ${f}:${hit} (mapfile/readarray -- bash 4+ builtin)
"
    done <<EOF
$(grep -nE '\b(mapfile|readarray)\b' "$f" | cut -d: -f1)
EOF

    # bash 4+ case-fold parameter expansion -- never allowlisted, anywhere
    while IFS= read -r hit; do
      [ -n "$hit" ] || continue
      n_banned_fail=$((n_banned_fail + 1))
      banned_fail_report="${banned_fail_report}  FAIL ${f}:${hit} (\${var^^}/\${var,,}/\${var^}/\${var,} case-fold -- bash 4+, never allowed)
"
    done <<EOF
$(grep -nE '\$\{[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?[,^]' "$f" | cut -d: -f1)
EOF

    # ---- GNU-tool landmines: warn only, never fail ----
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      n_warn=$((n_warn + 1))
      warn_report="${warn_report}  WARN ${f}:${line}
"
    done <<EOF
$(grep -nE 'grep[[:space:]]+(-[A-Za-z]*P|--perl-regexp)|sed[[:space:]]+-i|readlink[[:space:]]+-f|date[[:space:]]+-d|\bmapfile\b' "$f" | cut -d: -f1)
EOF
  fi

  IFS='
'
done
IFS="$OLDIFS"

# ---- shellcheck (optional) -------------------------------------------------
sc_available=0
sc_errors=0
sc_warnings=0
sc_other=0
if command -v shellcheck >/dev/null 2>&1; then
  sc_available=1
  # shellcheck disable=SC2086  # intentional: $FILES is a newline-separated
  # list and must word-split into separate positional args here.
  sc_out=$(shellcheck -S style $FILES 2>&1)
  sc_errors=$(printf '%s\n' "$sc_out" | grep -c '(error)')
  sc_warnings=$(printf '%s\n' "$sc_out" | grep -c '(warning)')
  sc_other=$(printf '%s\n' "$sc_out" | grep -Ec '\((info|style)\)')
fi

# ---- summary ---------------------------------------------------------------
echo "== check-portability.sh summary =="
echo "Scripts scanned: $n_scanned"
echo "Parse (bash -n): OK=$n_parse_ok FAIL=$n_parse_fail"
[ -n "$parse_fail_report" ] && printf '%s' "$parse_fail_report"

if [ "$sc_available" = 1 ]; then
  echo "shellcheck: errors=$sc_errors warnings=$sc_warnings info/style=$sc_other"
else
  echo "shellcheck: not found on PATH -- skipped"
fi

echo "Banned bash-4/GNU constructs: FAIL=$n_banned_fail allowlisted(issue #105)=$n_allowlisted"
[ -n "$banned_fail_report" ] && printf '%s' "$banned_fail_report"
[ -n "$allowlisted_report" ] && printf '%s' "$allowlisted_report"

echo "GNU-tool landmines (warn only): $n_warn"
[ -n "$warn_report" ] && printf '%s' "$warn_report"

result=0
[ "$n_parse_fail" -gt 0 ] && result=1
[ "$n_banned_fail" -gt 0 ] && result=1
[ "$sc_errors" -gt 0 ] && result=1

if [ "$result" -eq 0 ]; then
  echo "Result: PASS"
else
  echo "Result: FAIL"
fi
exit $result
