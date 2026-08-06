#!/usr/bin/env bash
# Neutrality guard for the public repo.
#
# The open-source strategy (KB zaentrum/opensource-strategy, candidate ADR-021,
# §6) says the public boundary is "enforced, not trusted" and describes exactly
# this gate. It had never been built. On 2026-08-06 the published operator image
# was found to carry `postgres.nalet.cloud`, `chino.beta.nalet.cloud` and
# `sso.nalet.cloud` — embedded in the binary via the chart templates the
# operator compiles in. Nothing was there to notice.
#
# Two rules:
#   1. No internal hostnames. A self-hoster reading our CRD descriptions should
#      see example.com, not somebody's production database.
#   2. No acquisition vocabulary. The public platform catalogs, processes and
#      streams files the user already owns; it does not name indexers,
#      trackers, usenet or specific download clients.
#
# Run: scripts/check-neutrality.sh [path]   (defaults to the repo root)
set -uo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
fail=0

# ── allowlist ────────────────────────────────────────────────────────────────
# The demo overlay IS nalet's own public reference deployment — its hostnames
# are the real, intentionally-published addresses of zaentrum.demo.nalet.cloud,
# not a leak. Everything else must be neutral.
#
# Keep this list short and justified. A growing allowlist means the rule is
# wrong, not that the exceptions are right.
allow_hosts=(
  'deploy/overlays/demo/'                        # nalet's own published demo deployment
  'operator/platform/chart/values-demo.yaml'     # ditto — the demo profile's real address
  'platform/keycloak/README.md'                  # documents that demo deployment
  'scripts/check-neutrality.sh'                  # this file lists the patterns it bans
)

# A line carrying this marker is skipped. Use it where the word is the POINT —
# a test asserting the render must NOT contain "qbittorrent" has to name it, and
# an OLM description promising "no indexer integrations" has to say "indexer".
# Per-line, not per-file: an exception must not quietly widen to its neighbours.
marker='neutrality-guard:allow'

is_allowed() {
  local f="$1" a
  for a in "${allow_hosts[@]}"; do
    [[ "$f" == *"$a"* ]] && return 0
  done
  return 1
}

# Only scan tracked files: build output and vendored deps are not ours to police.
# Read with a while-loop rather than mapfile — macOS ships bash 3.2, where
# mapfile does not exist, and this script has to run for a human locally as
# well as on an ubuntu runner.
files=()
while IFS= read -r f; do files+=("$f"); done < <(cd "$root" && git ls-files)

echo "neutrality guard: scanning ${#files[@]} tracked files"

# ── rule 1: internal hostnames ───────────────────────────────────────────────
host_re='[A-Za-z0-9._-]*\.(nalet\.cloud|implentic\.com)'
for f in "${files[@]}"; do
  is_allowed "$f" && continue
  [[ -f "$root/$f" ]] || continue
  hits=$(grep -nE "$host_re" "$root/$f" 2>/dev/null | grep -vF "$marker" | cut -c1-130)
  if [[ -n "$hits" ]]; then
    echo "FAIL internal hostname in $f"
    echo "$hits" | sed 's/^/        /'
    fail=1
  fi
done

# ── rule 2a: named acquisition tools — banned EVERYWHERE ─────────────────────
# Naming a specific client or indexer app is an integration signal wherever it
# appears, prose included.
tool_re='\b(nzbget|qbittorrent|jdownloader|odownloader|sonarr|radarr|prowlarr|jackett|transmission|deluge)\b'
for f in "${files[@]}"; do
  is_allowed "$f" && continue
  [[ -f "$root/$f" ]] || continue
  hits=$(grep -nEi "$tool_re" "$root/$f" 2>/dev/null | grep -vF "$marker" | cut -c1-130)
  if [[ -n "$hits" ]]; then
    echo "FAIL named acquisition tool in $f"
    echo "$hits" | sed 's/^/        /'
    fail=1
  fi
done

# ── rule 2b: generic acquisition vocabulary — code and config only ───────────
# Prose MUST be able to use these words, because stating the boundary is the
# whole point: README.md says "no indexer integrations" and CONTRIBUTING.md
# declines PRs that add "indexer/tracker integrations". A guard that fails on
# those would delete the project's own statement of what it refuses to do — the
# first version of this script did exactly that. So generic vocabulary is only
# a failure in code and config, where the word implies a feature rather than a
# promise not to build one.
vocab_re='\b(usenet|nzb|torrent|tracker|indexer|scraper)\b'
for f in "${files[@]}"; do
  is_allowed "$f" && continue
  [[ -f "$root/$f" ]] || continue
  case "$f" in
    *.md|*.txt|LICENSE*|*/docs/*) continue ;;
  esac
  hits=$(grep -nEi "$vocab_re" "$root/$f" 2>/dev/null | grep -vF "$marker" | cut -c1-130)
  if [[ -n "$hits" ]]; then
    echo "FAIL acquisition vocabulary in $f"
    echo "$hits" | sed 's/^/        /'
    fail=1
  fi
done

if [[ $fail -eq 0 ]]; then
  echo "neutrality guard: clean"
else
  echo ""
  echo "The public repo must not carry internal hostnames or acquisition"
  echo "vocabulary. Use example.com / a neutral placeholder, or move the code"
  echo "to the private side. If an exception is genuinely correct, add it to"
  echo "allow_hosts in this script WITH a reason."
fi
exit $fail
