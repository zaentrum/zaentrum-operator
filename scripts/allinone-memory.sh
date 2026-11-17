#!/usr/bin/env bash
# What the identity provider's pod is charged for while the all-in-one boots:
# its cgroup and each of its containers', once a second, read on the
# appliance (the container the boot job runs, named zaentrum).
#
#   allinone-memory.sh sample <file>   until stopped: one row per cgroup and
#                                      second, from the moment the pod exists
#   allinone-memory.sh report <file>   each field's maximum per cgroup, and the
#                                      20 rows before each end of the
#                                      Keycloak container (a restart)
#
# A row: seconds since the epoch, the cgroup (pod, or its container's id),
# memory.max, memory.current, then these fields of memory.stat, in bytes, as
# the kernel names them (a field the kernel does not have is -):
FIELDS="anon file shmem kernel kernel_stack slab pagetables sock file_dirty file_writeback"
# The Keycloak container is the one whose memory.max is the chart's limit.
LIMIT=${KEYCLOAK_LIMIT:-1073741824}
set -u

k() { docker exec zaentrum kubectl "$@"; }

sample() {
  local out=$1 uid="" dir="" since=0
  echo "time cgroup max current $FIELDS" > "$out"
  while true; do
    # The pod's uid, again now and then: a pod made anew has another.
    if [ -z "$uid" ] || [ $(( $(date +%s) - since )) -ge 30 ]; then
      uid=$(k -n zaentrum get pods -l app=keycloak -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null || true)
      since=$(date +%s); dir=""
      [ -n "$uid" ] && dir=$(docker exec zaentrum sh -c \
        'find /sys/fs/cgroup -maxdepth 4 -type d \( -name "*pod$1*" -o -name "*pod$2*" \) 2>/dev/null | head -1' \
        sh "$uid" "${uid//-/_}" 2>/dev/null || true)
    fi
    if [ -n "$dir" ]; then
      docker exec zaentrum sh -c '
        cd "$1" 2>/dev/null || exit 0
        for c in . *; do
          [ -f "$c/memory.stat" ] || continue
          echo "== $c $(cat "$c/memory.max" 2>/dev/null) $(cat "$c/memory.current" 2>/dev/null)"
          cat "$c/memory.stat"
        done' sh "$dir" 2>/dev/null |
        awk -v t="$(date +%s)" -v fields="$FIELDS" '
          function flush() { if (cg == "") return; line = t " " cg " " mx " " cur
            for (i = 1; i <= n; i++) line = line " " ((f[i] in v) ? v[f[i]] : "-"); print line }
          BEGIN { n = split(fields, f, " ") }
          /^== / { flush(); delete v; cg = ($2 == ".") ? "pod" : substr($2, 1, 12); mx = $3; cur = $4; next }
          { v[$1] = $2 }
          END { flush() }' >> "$out"
    fi
    sleep 1
  done
}

report() {
  local in=$1
  [ -s "$in" ] || { echo "no samples"; return 0; }
  echo "Samples: $(($(wc -l < "$in") - 1)) rows, $(awk 'NR > 1 && !($1 in s) { s[$1]; n++ } END { print n + 0 }' "$in") seconds."
  echo "Each field's maximum, MiB, per cgroup (memory.max in MiB, or max):"
  awk -v fields="$FIELDS" '
    BEGIN { n = split("current " fields, f, " ") }
    NR == 1 { for (i = 1; i <= NF; i++) col[$i] = i; next }
    { cg = $2; seen[cg]; mx[cg] = $3
      for (i = 1; i <= n; i++) { x = $(col[f[i]]); if (x != "-" && x + 0 > m[cg, f[i]]) m[cg, f[i]] = x + 0 } }
    END {
      h = sprintf("%-14s %8s", "cgroup", "max"); for (i = 1; i <= n; i++) h = h sprintf(" %14s", f[i]); print h
      for (cg in seen) { l = sprintf("%-14s %8s", cg, (mx[cg] == "max") ? "max" : sprintf("%d", mx[cg] / 1048576))
        for (i = 1; i <= n; i++) l = l sprintf(" %14.1f", m[cg, f[i]] / 1048576); print l } }' "$in"
  # A Keycloak container whose rows stop before the sampling does has ended:
  # killed or restarted. Its last 20 rows, and the pod's of the same seconds.
  local last ended
  last=$(awk 'NR > 1 { t = $1 } END { print t }' "$in")
  ended=$(awk -v lim="$LIMIT" -v last="$last" 'NR > 1 && $3 == lim { end[$2] = $1 }
    END { for (c in end) if (end[c] < last - 2) print end[c], c }' "$in" | sort -n | awk '{ print $2 }')
  for c in $ended; do
    echo "The 20 seconds before Keycloak container $c ended (MiB; the pod's row first in each second):"
    awk -v c="$c" -v fields="$FIELDS" '
      NR == 1 { print "time    cgroup       current " fields; next }
      $2 == c { ts[++k] = $1 } { row[$1, $2] = $0 }
      END { from = (k > 20) ? k - 19 : 1
        for (j = from; j <= k; j++) for (g = 0; g < 2; g++) { key = ts[j] SUBSEP ((g == 0) ? "pod" : c)
          if (!(key in row)) continue; nr = split(row[key], r, " ")
          l = sprintf("%s %-12s", r[1], r[2]); for (i = 4; i <= nr; i++) l = l sprintf(" %8s", (r[i] == "-") ? "-" : sprintf("%.1f", r[i] / 1048576)); print l } }' "$in"
  done
  [ -n "$ended" ] || echo "No Keycloak container ended while it was sampled."
}

case "${1:-}" in
  sample) sample "$2" ;;
  report) report "$2" ;;
  *) echo "usage: $0 sample|report <file>" >&2; exit 2 ;;
esac
