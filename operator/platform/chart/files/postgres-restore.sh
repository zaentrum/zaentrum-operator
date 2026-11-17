#!/bin/bash
# Restores the bundled Postgres from a dump the backup CronJob made
# (files/postgres-backup.sh). It runs as the restore Job
# (templates/postgres-restore.yaml), from the image the Postgres runs, over the
# network, once the database's clients are stopped — the operator stops them
# for it and starts them again after.
#
# First it makes sure the dump is whole, and refuses — exit 3, nothing changed
# — when it is not: the directory is there, SHA256SUMS names globals.sql and a
# .dump for every database the platform has, and backup.json where the set has
# one (a set made before sets held it has none), nothing else is beside them,
# every checksum matches, and pg_restore reads every dump's table of contents.
# backup.json is checked as a file of the set and read no further: the operator
# checked the set's version before anything stopped. Then
# it waits for every other session to leave the platform's databases, makes the
# roles of the dump the Postgres lacks (the superuser is never touched: its
# password stays Secret zaentrum-db's), and recreates each database from its
# dump — pg_restore --clean --create, stopping at the first error — and last
# checks each holds the tables its dump lists. Into a Postgres that already
# holds the databases, or into an empty one alike.
#
# A restore that fails after the first database was recreated leaves the
# databases partly restored; restoring the same dump again finishes it.
#
# Secrets travel in the environment only (PGPASSWORD). The termination message
# is what was restored, or the reason it was not.
#
# Environment (set by the Job):
#   PGHOST, PGUSER, PGPASSWORD   the bundled Postgres and its superuser
#   DUMP                         the dump's name, e.g. 2026-10-04T00-00-05Z
#   DATABASES                    the platform's databases, a name a line
#   BACKUP_DIR                   where the claim is mounted
set -Eeuo pipefail

termlog=${TERMINATION_LOG:-/dev/termination-log}
wait_seconds=${PG_WAIT_SECONDS:-300}
client_wait_seconds=${CLIENT_WAIT_SECONDS:-300}
dir=${BACKUP_DIR:-/backups}

fail() {
	trap - ERR
	printf '%s' "$*" >"$termlog" 2>/dev/null || true
	printf 'postgres-restore: %s\n' "$*" >&2
	exit 1
}
# refuse ends a restore that has changed nothing, and says so.
refuse() {
	trap - ERR
	printf 'refused: %s; nothing was changed' "$*" >"$termlog" 2>/dev/null || true
	printf 'postgres-restore: refused: %s; nothing was changed\n' "$*" >&2
	exit 3
}
trap 'fail "the restore stopped at line $LINENO of postgres-restore.sh; the Job log says why"' ERR

say() { printf 'postgres-restore: %s\n' "$*"; }

[ -n "${PGUSER:-}" ] && [ -n "${PGPASSWORD:-}" ] || fail "Secret zaentrum-db holds no user and password"
# Each line of DATABASES is a database's name, whole.
databases=()
while IFS= read -r db; do
	[ -z "$db" ] || databases+=("$db")
done <<<"${DATABASES:-}"
[ ${#databases[@]} -gt 0 ] || fail "DATABASES names no database"
case ${DUMP:-} in
[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]-[0-9][0-9]-[0-9][0-9]Z) ;;
*) refuse "'${DUMP:-}' is not the name of a dump (such as 2026-10-04T00-00-05Z)" ;;
esac
src=$dir/$DUMP
if [ ! -d "$src" ]; then
	have=$(cd "$dir" 2>/dev/null && find . -mindepth 1 -maxdepth 1 -type d -name '[0-9]*Z' | sed 's|^\./||' | LC_ALL=C sort -r | head -n 10 | tr '\n' ' ')
	refuse "there is no dump $DUMP on the claim (it holds: ${have:-none})"
fi

# ── the dump is whole ───────────────────────────────────────────────────────
[ -f "$src/SHA256SUMS" ] || refuse "$DUMP has no SHA256SUMS"
# A line of SHA256SUMS is a checksum, a space, a space or a star, and the
# file's name, which may hold spaces.
listed=$(sed -E 's/^[0-9a-fA-F]+ [ *]//' "$src/SHA256SUMS" | LC_ALL=C sort)
there=$(cd "$src" && find . -mindepth 1 -maxdepth 1 ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort)
# backup.json belongs to the set where it is there or listed: then both have
# to hold it.
record=""
if grep -qxF backup.json <<<"$listed" || grep -qxF backup.json <<<"$there"; then
	record=backup.json
fi
want=$( (echo globals.sql; printf '%s.dump\n' "${databases[@]}"; [ -z "$record" ] || echo "$record") | LC_ALL=C sort)
[ "$listed" = "$want" ] ||
	refuse "$DUMP's SHA256SUMS lists [$(echo $listed)], not the files of the databases [$(echo $want)]"
[ "$there" = "$want" ] ||
	refuse "$DUMP holds [$(echo $there)], not the files its SHA256SUMS lists"
if ! bad=$(cd "$src" && sha256sum -c SHA256SUMS 2>&1); then
	refuse "$DUMP does not match its checksums: $(printf '%s' "$bad" | grep -v ': OK$' | tr '\n' ' ' | cut -c1-200)"
fi
for db in "${databases[@]}"; do
	pg_restore --list "$src/$db.dump" >/dev/null 2>&1 || refuse "pg_restore cannot read $db.dump of $DUMP"
done
say "$DUMP is whole: its checksums match"

# ── nobody else is in the databases ─────────────────────────────────────────
deadline=$((SECONDS + wait_seconds))
until pg_isready -q; do
	[ "$SECONDS" -lt "$deadline" ] || refuse "the Postgres at ${PGHOST:-localhost} does not answer"
	sleep 2
done
q() { psql -X -v ON_ERROR_STOP=1 -At -d postgres "$@"; }
# The databases' names are one value, a name a line, which psql quotes
# (:'dbs'); never text of the statement, which psql reads from its input.
names=$(printf '%s\n' "${databases[@]}")
sessions="SELECT string_agg(DISTINCT datname || ' (' || coalesce(nullif(application_name, ''), client_addr::text, 'local') || ')', ', ')
	FROM pg_stat_activity WHERE datname = ANY (string_to_array(:'dbs', E'\n')) AND pid <> pg_backend_pid();"
deadline=$((SECONDS + client_wait_seconds))
while :; do
	others=$(printf '%s\n' "$sessions" | q -v "dbs=$names")
	[ -n "$others" ] || break
	[ "$SECONDS" -lt "$deadline" ] || refuse "the databases still have clients: $others"
	sleep 3
done

# ── the roles, then each database ───────────────────────────────────────────
roles=$(q -c "SELECT rolname FROM pg_roles")
grep -v -E "^(CREATE|ALTER) ROLE \"?${PGUSER}\"?[ ;]" "$src/globals.sql" |
	while IFS= read -r line; do
		if [[ $line =~ ^CREATE\ ROLE\ \"?([^\";]+)\"?\;$ ]] && grep -qxF "${BASH_REMATCH[1]}" <<<"$roles"; then
			continue
		fi
		printf '%s\n' "$line"
	done | psql -X -v ON_ERROR_STOP=1 -q -d postgres -o /dev/null ||
	fail "cannot make the roles of $DUMP"

restored=()
for db in "${databases[@]}"; do
	pg_restore --clean --if-exists --create --exit-on-error --no-password -d postgres "$src/$db.dump" ||
		fail "cannot restore the database $db from $DUMP (${#restored[@]} of the databases were restored before it: ${restored[*]:-none}); restore it again"
	# The table of contents lists each table as "TABLE", its rows as "TABLE DATA".
	# psql connects to the database by its name in PGDATABASE, which it takes
	# as the database's name alone, whatever it holds.
	want_tables=$(pg_restore --list "$src/$db.dump" | grep -E '^[0-9]+; [0-9]+ [0-9]+ TABLE ' | grep -c -v ' TABLE DATA ' || true)
	got_tables=$(PGDATABASE=$db psql -X -v ON_ERROR_STOP=1 -At -c "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'")
	[ "$want_tables" = "$got_tables" ] ||
		fail "the restored $db holds $got_tables tables, its dump $want_tables"
	restored+=("$db ($got_tables tables)")
	say "restored $db: $got_tables tables"
done

printf -v line '%s, ' "${restored[@]}"
line="restored ${line%, } from $DUMP"
say "$line"
printf '%.3000s' "$line" >"$termlog" 2>/dev/null || true
