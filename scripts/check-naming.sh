#!/usr/bin/env bash
# Naming guard — mechanical enforcement of ADR-0015 (Zerker Gateway).
#
# Two rules, both of which review has already failed to catch by eye:
#
#   1. No `farcaster` anywhere, except a small pinned set of deliberate
#      exceptions. The set is pinned BY COUNT, so this fires both when someone
#      adds a new stale reference and when someone "tidies up" one of the
#      legacy guards — deleting those does not remove a string, it removes a
#      protection (see ADR-0015 Decision 4).
#
#   2. In user-facing docs, every `Zerker` must be an approved form. Bare
#      "Zerker" is the COMPANY (which also ships Treeship and Zmem); the
#      product is "Zerker Gateway" (ADR-0015 Decision 1).
#
# Run: scripts/check-naming.sh
set -euo pipefail

cd "$(dirname "$0")/.."
fail=0

# ---------------------------------------------------------------------------
# Rule 1 — `farcaster` only where deliberately kept.
#
# "<path>:<expected count>". Update ONLY with a reason in the commit message.
# ---------------------------------------------------------------------------
ALLOWED=(
  "AGENTS.md:1"                              # Hyperion origin story, kept as history
  "gateway/db/migrate.go:2"                  # legacy advisory lock — mixed-version deploys
  "gateway/internal/proxy/headers.go:3"      # legacy X-Farcaster-* strip — anti-spoof
  "gateway/internal/proxy/proxy_test.go:6"   # regression test for the above
  "scripts/check-naming.sh:0"                # this file describes the rule; not counted
)

allowed_count_for() {
  local path="$1" entry
  for entry in "${ALLOWED[@]}"; do
    if [ "${entry%:*}" = "$path" ]; then printf '%s' "${entry##*:}"; return; fi
  done
  printf '0'
}

# Every tracked file containing `farcaster`, case-insensitive, with its count.
# `git grep -I` skips binaries. This script excludes itself.
while IFS=: read -r path count; do
  [ -z "$path" ] && continue
  [ "$path" = "scripts/check-naming.sh" ] && continue
  want="$(allowed_count_for "$path")"
  if [ "$count" != "$want" ]; then
    if [ "$want" = "0" ]; then
      echo "FAIL  $path — $count 'farcaster' reference(s); the product is Zerker Gateway (ADR-0015)."
      git grep -in 'farcaster' -- "$path" | sed 's/^/        /'
    else
      echo "FAIL  $path — expected $want deliberate 'farcaster' reference(s), found $count."
      echo "        If you removed a legacy guard, read ADR-0015 Decision 4 first: those"
      echo "        strings carry a protection, they are not leftovers."
    fi
    fail=1
  fi
done < <(git grep -Ic -i 'farcaster' -- . || true)

# A pinned exception that disappears entirely never shows up above, so check
# for that too.
for entry in "${ALLOWED[@]}"; do
  path="${entry%:*}"; want="${entry##*:}"
  [ "$want" = "0" ] && continue
  if ! git ls-files --error-unmatch "$path" >/dev/null 2>&1; then
    echo "FAIL  $path is pinned in the naming allowlist but no longer tracked."
    fail=1
  elif ! git grep -qi 'farcaster' -- "$path"; then
    echo "FAIL  $path — pinned for $want deliberate 'farcaster' reference(s), found none."
    echo "        Removing them removes a protection (ADR-0015 Decision 4)."
    fail=1
  fi
done

# ---------------------------------------------------------------------------
# Rule 2 — user-facing docs say "Zerker Gateway", not "Zerker".
# ---------------------------------------------------------------------------
DOC_PATHS=(www README.md NOTICE)

# Approved forms. Anything else beginning "Zerker" is the product being called
# by the company's name.
strip_approved() {
  sed -E \
    -e 's/Zerker Gateway//g' \
    -e 's/Zerker Labs//g' \
    -e 's/Zerker stack//g' \
    -e 's/Zerker-(managed|operated|hosted)//g' \
    -e 's/X-Zerker-//g' \
    -e 's/ZERKER_//g' \
    -e 's/zerker\.ai//g' \
    -e 's/zerker-gateway//g' \
    -e 's/zerkerlabs//g'
}

while IFS= read -r line; do
  [ -z "$line" ] && continue
  echo "FAIL  bare 'Zerker' used as the product name — say 'Zerker Gateway' on first"
  echo "      use, then 'the gateway' (ADR-0015 Decision 1). Bare 'Zerker' is the company."
  echo "        $line"
  fail=1
done < <(git grep -n 'Zerker' -- "${DOC_PATHS[@]}" 2>/dev/null \
          | while IFS= read -r l; do
              if printf '%s' "$l" | strip_approved | grep -q 'Zerker'; then printf '%s\n' "$l"; fi
            done)

if [ "$fail" -eq 0 ]; then
  echo "naming OK — no stray 'farcaster'; user-facing docs say Zerker Gateway."
fi
exit "$fail"
