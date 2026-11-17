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
#   <db>.dump     each database in pg_dump's custom format, compressed, by
#                 the database's whole name: a name holds no slash, so each
#                 is a file of the set, and a dot in it stays in the file's
#                 name; the restore takes the names from DATABASES, never back
#                 from the files' names
#   backup.json   what made the set: platform, its first field (the resource's
#                 name and uid, the version that runs, the operator's), v (1),
#                 createdAt (when the run began), the databases with their
#                 tables and bytes, the resource's spec, the names of its
#                 Secrets and the addons. All but the time and the databases
#                 come from the ConfigMap zaentrum-backup, mounted at
#                 RECORD_DIR; without it the set has no backup.json, and the log
#                 says so
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
#   DATABASES                    the platform's databases, a name a line
#   BACKUP_DIR                   where the claim is mounted
#   RETENTION                    how many dumps to keep
#   RECORD_DIR                   the ConfigMap zaentrum-backup: platform,
#                                resource, secrets, addons
set -Eeuo pipefail

termlog=${TERMINATION_LOG:-/dev/termination-log}
wait_seconds=${PG_WAIT_SECONDS:-300}
dir=${BACKUP_DIR:-/backups}
record=${RECORD_DIR:-/record}
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
# Each line of DATABASES is a database's name, whole.
databases=()
while IFS= read -r db; do
	[ -z "$db" ] || databases+=("$db")
done <<<"${DATABASES:-}"
[ ${#databases[@]} -gt 0 ] || fail "DATABASES names no database"
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

dbs_json=""
files=(globals.sql)
for db in "${databases[@]}"; do
	# The name is a value, which psql quotes (:'db'); never text of the
	# statement, which psql reads from its input to put it in. The tools
	# connect to the database by its name in PGDATABASE, which they take as
	# the database's name alone, whatever it holds.
	[ -n "$(printf '%s\n' "SELECT 1 FROM pg_database WHERE datname = :'db';" | q -d postgres -v "db=$db")" ] ||
		fail "the Postgres has no database $db"
	PGDATABASE=$db pg_dump --format=custom --no-password -f "$partial/$db.dump" ||
		fail "cannot dump the database $db"
	tables=$(PGDATABASE=$db q -c "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'")
	size=$(wc -c <"$partial/$db.dump" | tr -d ' ')
	dbs_json+="${dbs_json:+,}$(js "$db"):{\"tables\":$tables,\"bytes\":$size}"
	files+=("$db.dump")
	say "dumped $db: $tables tables, $size bytes"
done

# What made the set: the record of the ConfigMap, platform first, with when the
# run began and the databases. Each part is JSON as the operator wrote it.
recorded() {
	local key
	for key in platform resource secrets addons; do
		[ -s "$record/$key" ] || return 1
	done
	case $(cat "$record/platform") in '{'*'}') ;; *) return 1 ;; esac
	case $(cat "$record/resource") in '{'*'}') ;; *) return 1 ;; esac
	case $(cat "$record/secrets") in '['*']') ;; *) return 1 ;; esac
	case $(cat "$record/addons") in '['*']') ;; *) return 1 ;; esac
}
if recorded; then
	created="${name:0:13}:${name:14:2}:${name:17:2}Z"
	printf '{"platform":%s,"v":1,"createdAt":"%s","databases":{%s},"resource":%s,"secrets":%s,"addons":%s}\n' \
		"$(cat "$record/platform")" "$created" "$dbs_json" "$(cat "$record/resource")" \
		"$(cat "$record/secrets")" "$(cat "$record/addons")" >"$partial/backup.json" ||
		fail "cannot write backup.json"
	files+=(backup.json)
else
	say "the ConfigMap zaentrum-backup holds no record of the platform: the set is made without backup.json"
fi

(cd "$partial" && sha256sum -- "${files[@]}" >SHA256SUMS) || fail "cannot write the checksums"
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
