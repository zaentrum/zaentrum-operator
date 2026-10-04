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
# Three rules:
#   1. No internal hostnames. A self-hoster reading our CRD descriptions should
#      see example.com, not somebody's production database.
#   2. No acquisition vocabulary. The public platform catalogs, processes and
#      streams files the user already owns; it does not name indexers,
#      trackers, usenet or specific download clients.
#   3. No other media products by name. The platform describes itself, not
#      what it might replace.
#
# Before it scans, the guard checks its own patterns against lines it must and
# must not flag (self_test, below); a pattern that has gone blind fails the run.
#
# Run: scripts/check-neutrality.sh [path]   (defaults to the repo root)
#      scripts/check-neutrality.sh --self-test   (the patterns only)
set -uo pipefail

# ── the patterns ─────────────────────────────────────────────────────────────
# A name counts where what stands on either side of it is not a letter or a
# digit. \b would not do: it counts "_" as part of a word, so an environment
# variable such as ODOWNLOADER_API_URL went through \bodownloader\b unseen. A
# letter or a digit on either side still keeps a name out of longer words —
# the third rule's names are not in "complex" or "multiplex". (A name run into
# camelCase, odownloaderUrl, is not caught either; the guard trades that for no
# false positives.) Plain POSIX classes: BSD grep on macOS reads them as GNU
# grep on the CI runner does.
word() { printf '(^|[^[:alnum:]])(%s)([^[:alnum:]]|$)' "$1"; }

host_re='[A-Za-z0-9._-]*\.(nalet\.cloud|implentic\.com)'
# Named acquisition tools and components — banned everywhere, prose included.
tool_re=$(word 'nzbget|qbittorrent|jdownloader|odownloader|sonarr|radarr|prowlarr|jackett|transmission|deluge|download[-_]?gateway')
# Generic acquisition vocabulary — code and config only (rule 2b below).
vocab_re=$(word 'usenet|nzb|torrent|tracker|indexer|scraper')
# Other media products, put together from pieces so that this file, which the
# scan skips, does not name them either.
p1='jelly''fin' p2='pl''ex' p3='em''by' p4='ko''di'
product_re=$(word "$p1|$p2|$p3|$p4")

# ── self-test ────────────────────────────────────────────────────────────────
self_test() {
  local bad=0 upper
  expect() { # expect <name> <pattern> <hit|miss> <line>
    local got=miss
    printf '%s\n' "$4" | grep -qiE "$2" && got=hit
    if [[ "$got" != "$3" ]]; then
      echo "SELF-TEST FAIL: the $1 pattern should $([[ $3 == hit ]] && echo flag || echo pass) this line: $4"
      bad=1
    fi
  }
  # Joined by "_" — the names \b let through.
  expect tool "$tool_re" hit '        - name: ODOWNLOADER_API_URL'
  expect tool "$tool_re" hit 'ODOWNLOADER_API_TOKEN=""'
  expect tool "$tool_re" hit '        - name: DOWNLOAD_GATEWAY_URL'
  expect tool "$tool_re" hit 'DOWNLOAD_GATEWAY_EVENTS_ENABLED: "false"'
  expect tool "$tool_re" hit 'sonarr_api_key: x'
  expect tool "$tool_re" hit 'QBITTORRENT_HOST'
  # The forms \b caught, still caught.
  expect tool "$tool_re" hit 'url: http://download-gateway:8080'
  expect tool "$tool_re" hit 'image: ghcr.io/example/radarr:latest'
  expect tool "$tool_re" hit 'Prowlarr'
  expect tool "$tool_re" hit 'odownloader'
  # Longer words are other words.
  expect tool "$tool_re" miss 'retransmissions=3'
  expect tool "$tool_re" miss 'download the chart'
  expect vocab "$vocab_re" hit 'TORRENT_DIR=/data'
  expect vocab "$vocab_re" hit 'indexer_url: http://x'
  expect vocab "$vocab_re" hit 'nzb-watch'
  expect vocab "$vocab_re" hit 'type Tracker struct'
  expect vocab "$vocab_re" miss 'torrential'
  expect vocab "$vocab_re" miss 'reindexers'
  upper=$(printf '%s' "$p2" | tr '[:lower:]' '[:upper:]')
  expect product "$product_re" hit "${upper}_TOKEN"
  expect product "$product_re" hit "url: http://${p1}:8096"
  expect product "$product_re" hit "the ${p3} client"
  expect product "$product_re" miss 'complex'
  expect product "$product_re" miss 'multiplex'
  expect product "$product_re" miss 'COMPLEX_QUERY=1'
  expect product "$product_re" miss 'perplexed'
  expect product "$product_re" miss 'kodiak'
  expect host "$host_re" hit "db.nalet"".cloud"
  expect host "$host_re" miss 'media.example.org'
  return $bad
}

if ! self_test; then
  echo "neutrality guard: its own patterns are wrong; fix them before trusting a scan"
  exit 2
fi
if [[ "${1:-}" == "--self-test" ]]; then
  echo "neutrality guard: self-test passed"
  exit 0
fi

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

# scan <label> <pattern> <grep case flag> [prose]: every tracked file but the
# allowed ones; with prose=skip, code and config only.
scan() {
  local label=$1 re=$2 icase=$3 prose=${4:-} f hits
  for f in "${files[@]}"; do
    is_allowed "$f" && continue
    [[ -f "$root/$f" ]] || continue
    if [[ "$prose" == skip ]]; then
      case "$f" in
        *.md|*.txt|LICENSE*|*/docs/*) continue ;;
      esac
    fi
    hits=$(grep -nE $icase "$re" "$root/$f" 2>/dev/null | grep -vF "$marker" | cut -c1-130)
    if [[ -n "$hits" ]]; then
      echo "FAIL $label in $f"
      echo "$hits" | sed 's/^/        /'
      fail=1
    fi
  done
}

# ── rule 1: internal hostnames ───────────────────────────────────────────────
scan "internal hostname" "$host_re" ""

# ── rule 2a: named acquisition tools — banned EVERYWHERE ─────────────────────
# Naming a specific client or indexer app, or a component built to drive one,
# is an integration signal wherever it appears, prose included.
scan "named acquisition tool" "$tool_re" -i

# ── rule 2b: generic acquisition vocabulary — code and config only ───────────
# Prose MUST be able to use these words, because stating the boundary is the
# whole point: README.md says "no indexer integrations" and CONTRIBUTING.md
# declines PRs that add "indexer/tracker integrations". A guard that fails on
# those would delete the project's own statement of what it refuses to do — the
# first version of this script did exactly that. So generic vocabulary is only
# a failure in code and config, where the word implies a feature rather than a
# promise not to build one.
scan "acquisition vocabulary" "$vocab_re" -i skip

# ── rule 3: other media products — banned everywhere ─────────────────────────
scan "another media product's name" "$product_re" -i

if [[ $fail -eq 0 ]]; then
  echo "neutrality guard: clean"
else
  echo ""
  echo "The public repo must not carry internal hostnames, acquisition"
  echo "vocabulary or other media products' names. Use example.com / a neutral"
  echo "placeholder, or move the code to the private side. If an exception is"
  echo "genuinely correct, add it to allow_hosts in this script WITH a reason."
fi
exit $fail
