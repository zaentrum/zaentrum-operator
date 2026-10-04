#!/bin/bash
# Backs up the bundled Postgres: every platform database, dumped into a
# directory of its own on the claim backups, with a checksum of every file. It
# runs as the backup CronJob's Job (templates/backup.yaml), from the image the
# Postgres runs, over the network, while the platform keeps serving: each
# database's dump is one consistent snapshot of it (pg_dump's own), the
# databases are dumped one after another.
#
# A dump is a directory named after the moment it began, in UTC — for instance
# 2026-10-04T00-00-05Z, the name a restore asks for — and holds
#   globals.sql   the roles and their memberships, but the superuser's
#                 (pg_dumpall --globals-only): what the databases' grants need
#   <db>.dump     each database in pg_dump's custom format, compressed
#   SHA256SUMS    the sha256 of each of those files
# It is written as .partial-<name> and renamed once whole, so a dump without
# the dot is complete. Then the newest RETENTION dumps stay, older ones go, and
# so does what a run that died left behind.
#
# Secrets travel in the environment only (PGPASSWORD, from Secret
# zaentrum-db). On success the termination message is a summary in JSON — the
# dump, its size, the databases, the dumps kept — which the operator reads into
# status.backup; on failure it is the reason, and only the reason.
#
# Environment (set by the Job):
#   PGHOST, PGUSER, PGPASSWORD   the bundled Postgres and its superuser
#   DATABASES                    the platform's databases, space-separated
#   BACKUP_DIR                   where the claim is mounted
#   RETENTION                    how many dumps to keep
set -Eeuo pipefail

termlog=${TERMINATION_LOG:-/dev/termination-log}
wait_seconds=${PG_WAIT_SECONDS:-300}
dir=${BACKUP_DIR:-/backups}
partial=""

fail() {
	trap - ERR
	[ -z "$partial" ] || rm -rf "$partial"
	printf '%s' "$*" >"$termlog" 2>/dev/null || true
	printf 'postgres-backup: %s\n' "$*" >&2
	exit 1
}
trap 'fail "the backup stopped at line $LINENO of postgres-backup.sh; the Job log says why"' ERR

say() { printf 'postgres-backup: %s\n' "$*"; }

# js quotes a value as a JSON string.
js() {
	local s=$1
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	printf '"%s"' "$s"
}

[ -n "${PGUSER:-}" ] && [ -n "${PGPASSWORD:-}" ] || fail "Secret zaentrum-db holds no user and password"
[ -n "${DATABASES:-}" ] || fail "DATABASES names no database"
case ${RETENTION:-} in
'' | *[!0-9]* | 0) fail "RETENTION is not a number of dumps to keep: '${RETENTION:-}'" ;;
esac
[ -d "$dir" ] && [ -w "$dir" ] || fail "$dir is not a directory this backup can write"

deadline=$((SECONDS + wait_seconds))
until pg_isready -q; do
	[ "$SECONDS" -lt "$deadline" ] || fail "the Postgres at ${PGHOST:-localhost} does not answer"
	sleep 2
done
q() { psql -X -v ON_ERROR_STOP=1 -At "$@"; }

name=$(date -u +%Y-%m-%dT%H-%M-%SZ)
[ ! -e "$dir/$name" ] || fail "a dump named $name is there already"
partial=$dir/.partial-$name
mkdir "$partial"

# The roles, but the superuser: its password is Secret zaentrum-db's, which a
# restore never changes.
pg_dumpall --globals-only --no-tablespaces |
	grep -v -E "^(CREATE|ALTER) ROLE \"?${PGUSER}\"?[ ;]" >"$partial/globals.sql" ||
	fail "cannot dump the roles"

dbs_json="" files="globals.sql"
for db in $DATABASES; do
	[ -n "$(q -d postgres -c "SELECT 1 FROM pg_database WHERE datname = '$db'")" ] ||
		fail "the Postgres has no database $db"
	pg_dump --format=custom --no-password -d "$db" -f "$partial/$db.dump" ||
		fail "cannot dump the database $db"
	tables=$(q -d "$db" -c "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'")
	size=$(wc -c <"$partial/$db.dump" | tr -d ' ')
	dbs_json+="${dbs_json:+,}$(js "$db"):{\"tables\":$tables,\"bytes\":$size}"
	files+=" $db.dump"
	say "dumped $db: $tables tables, $size bytes"
done

# shellcheck disable=SC2086 # the file names are words
(cd "$partial" && sha256sum $files >SHA256SUMS) || fail "cannot write the checksums"
sync -f "$partial" 2>/dev/null || sync
mv "$partial" "$dir/$name"
partial=""
bytes=$(du -sk "$dir/$name" | cut -f1)
say "wrote $name"

# Keep the newest RETENTION dumps; remove the rest, and any unfinished one.
kept=()
while IFS= read -r d; do
	kept+=("$d")
done < <(cd "$dir" && find . -mindepth 1 -maxdepth 1 -type d -name '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]-[0-9][0-9]-[0-9][0-9]Z' |
	sed 's|^\./||' | LC_ALL=C sort -r)
for ((i = RETENTION; i < ${#kept[@]}; i++)); do
	rm -rf "${dir:?}/${kept[$i]}"
	say "removed ${kept[$i]}, older than the newest $RETENTION"
done
[ ${#kept[@]} -le "$RETENTION" ] || kept=("${kept[@]:0:$RETENTION}")
find "$dir" -mindepth 1 -maxdepth 1 -name '.partial-*' -exec rm -rf {} + 2>/dev/null || true

free=$(df -Pk "$dir" | awk 'NR == 2 { print $4 }')
kept_json=""
for d in "${kept[@]:0:50}"; do
	kept_json+="${kept_json:+,}$(js "$d")"
done
summary="{\"v\":1,\"dump\":$(js "$name"),\"bytes\":$((bytes * 1024)),\"databases\":{$dbs_json},\"kept\":[$kept_json],\"free\":$((free * 1024))}"
say "$summary"
printf '%s' "$summary" >"$termlog" 2>/dev/null || true
