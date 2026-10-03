#!/bin/bash
# Copies every database of the running bundled Postgres onto the volume it is
# about to move to. It runs as the migration Job (templates/postgres-migrate.yaml),
# from the image the Postgres runs, with the volume mounted where the Postgres
# will mount it, before anything switches: the Postgres keeps serving from where
# it is — an emptyDir, say — until this has succeeded.
#
# It initialises the volume the way the image's own entrypoint initialises an
# empty one, with the same superuser and password (Secret zaentrum-db), and
# fills it from the running Postgres instead of the init scripts: the roles
# (but the superuser, which initdb made), then each database, plain pg_dump
# into psql with ON_ERROR_STOP, so the first error fails the copy rather than
# being skipped. Then it checks that every database holds the tables the
# source holds, stops the server cleanly and records that the copy finished.
# When the Postgres starts on the volume it finds a database there and skips
# its initialisation.
#
# What it writes into, and what it never touches. The volume may be written
# when PGDATA is empty, or holds a copy this script made that no server has run
# on since — an unfinished one, or a finished one the Postgres never switched
# to, which is stale, as the source went on. Anything else — a database it did
# not write, one a server has run on since, a server running on it — makes it
# refuse, and change nothing.
#
# Writes made to the source while the copy runs, and until the Postgres has
# switched over, are not carried across: do it when the platform is quiet.
#
# Secrets travel in the environment only (POSTGRES_PASSWORD, from Secret
# zaentrum-db). On failure the reason, and only the reason, is the termination
# message; on success it is the summary of what was copied.
#
# Environment (set by the Job):
#   POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DB, PGDATA   as the Postgres has them
#   SOURCE_HOST   the running Postgres, e.g. postgres
#   TARGET        what the volume is called, for the summary
set -Eeuo pipefail

termlog=${TERMINATION_LOG:-/dev/termination-log}
wait_seconds=${SOURCE_WAIT_SECONDS:-300}
volume=$(dirname "$PGDATA")
started=$volume/.zaentrum-migrating
finished=$volume/.zaentrum-migrated

fail() {
	trap - ERR
	printf '%s' "$*" >"$termlog" 2>/dev/null || true
	printf 'postgres-migrate: %s\n' "$*" >&2
	exit 1
}
# A step that fails without saying why still leaves a reason: where.
trap 'fail "the copy stopped at line $LINENO of postgres-migrate.sh; the Job log says why"' ERR

say() { printf 'postgres-migrate: %s\n' "$*"; }

[ -n "${POSTGRES_USER:-}" ] && [ -n "${POSTGRES_PASSWORD:-}" ] ||
	fail "Secret zaentrum-db holds no user and password"
[ -n "${SOURCE_HOST:-}" ] || fail "no SOURCE_HOST to copy from"

# checkpoint is where the cluster on the volume last checkpointed: it moves
# whenever a server runs on it, if only to shut down.
checkpoint() {
	pg_controldata "$PGDATA" 2>/dev/null | sed -n 's/^Latest checkpoint location: *//p'
}

# state says what the volume holds, and so whether it may be written.
state() {
	if [ ! -d "$PGDATA" ] || [ -z "$(ls -A "$PGDATA" 2>/dev/null)" ]; then
		echo empty
	elif [ -e "$PGDATA/postmaster.pid" ]; then
		echo running
	elif [ -f "$finished" ]; then
		local recorded
		recorded=$(sed -n 's/^checkpoint=//p' "$finished")
		if [ -n "$recorded" ] && [ "$recorded" = "$(checkpoint)" ]; then
			echo unused-copy
		else
			echo used
		fi
	elif [ -f "$started" ]; then
		echo unfinished-copy
	else
		echo foreign
	fi
}

was=$(state)
case $was in
empty | unfinished-copy | unused-copy) ;;
running)
	fail "a Postgres is running on ${TARGET:-the volume} (postmaster.pid); nothing was changed"
	;;
used)
	fail "${TARGET:-the volume} holds a copy a Postgres has run on since; it is not overwritten"
	;;
*)
	fail "${TARGET:-the volume} already holds a database this migration did not write; it is not overwritten"
	;;
esac

# As root, as the image's entrypoint does: own the volume, then go on as the
# postgres user. (OpenShift runs the Job as the namespace's user already.)
if [ "$(id -u)" = 0 ]; then
	mkdir -p "$PGDATA"
	chown postgres:postgres "$volume" "$PGDATA"
	for marker in "$started" "$finished"; do
		[ ! -e "$marker" ] || chown postgres:postgres "$marker"
	done
	exec gosu postgres bash -c "$BASH_EXECUTION_STRING"
fi

[ "$was" = empty ] || say "an earlier copy on the volume was never used; it is made again"
rm -rf "$PGDATA" "$finished"
mkdir -p "$PGDATA"
printf 'started=%s\n' "$(date -u +%FT%TZ)" >"$started"

# The source must answer; it may be restarting.
export PGPASSWORD=$POSTGRES_PASSWORD
deadline=$((SECONDS + wait_seconds))
until pg_isready -q -h "$SOURCE_HOST" -U "$POSTGRES_USER"; do
	[ "$SECONDS" -lt "$deadline" ] || fail "the Postgres at $SOURCE_HOST does not answer"
	sleep 2
done
src() { psql -h "$SOURCE_HOST" -U "$POSTGRES_USER" -X -v ON_ERROR_STOP=1 -At "$@"; }
dbs=$(src -d postgres -c "SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY 1") ||
	fail "cannot list the databases at $SOURCE_HOST"

# The volume, made the way the image makes an empty one at its first start —
# with the image's own functions, which are not written for set -u.
# shellcheck disable=SC1091
source /usr/local/bin/docker-entrypoint.sh
set +u
docker_setup_env
docker_create_db_directories
docker_verify_minimum_env
docker_init_database_dir >/dev/null
pg_setup_hba_conf postgres
docker_temp_server_start postgres >/dev/null
docker_setup_db >/dev/null
set -u
dst() { PGHOST= PGHOSTADDR= psql -U "$POSTGRES_USER" -X -v ON_ERROR_STOP=1 -q "$@"; }

# Every role but the superuser, which initdb made with the same password.
pg_dumpall -h "$SOURCE_HOST" -U "$POSTGRES_USER" --globals-only --no-tablespaces |
	grep -v -E "^(CREATE|ALTER) ROLE \"?$POSTGRES_USER\"?[ ;]" |
	dst -d postgres -o /dev/null ||
	fail "cannot copy the roles from $SOURCE_HOST"

summary=()
total=0
while IFS= read -r db; do
	[ -n "$db" ] || continue
	owner=$(src -d postgres -c "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = '$db'")
	if [ -z "$(dst -d postgres -At -c "SELECT 1 FROM pg_database WHERE datname = '$db'")" ]; then
		dst -d postgres -c "CREATE DATABASE \"$db\" OWNER \"$owner\"" ||
			fail "cannot create the database $db"
	fi
	pg_dump -h "$SOURCE_HOST" -U "$POSTGRES_USER" --no-password -d "$db" | dst -d "$db" -o /dev/null ||
		fail "cannot copy the database $db"
	have=$(src -d "$db" -c "SELECT schemaname||'.'||relname FROM pg_stat_user_tables ORDER BY 1")
	got=$(dst -d "$db" -At -c "SELECT schemaname||'.'||relname FROM pg_stat_user_tables ORDER BY 1")
	[ "$have" = "$got" ] ||
		fail "the copy of $db holds other tables than its source"
	tables=$(dst -d "$db" -At -c "SELECT count(*) FROM pg_stat_user_tables")
	rows=$(dst -d "$db" -At -c "SELECT coalesce(sum(n_live_tup), 0) FROM pg_stat_user_tables")
	size=$(dst -d "$db" -At -c "SELECT pg_database_size(current_database())")
	total=$((total + size))
	summary+=("$db ($tables tables, $rows rows)")
	say "copied $db: $tables tables, $rows rows"
done <<<"$dbs"

set +u
docker_temp_server_stop >/dev/null
set -u
printf 'checkpoint=%s\nfinished=%s\n' "$(checkpoint)" "$(date -u +%FT%TZ)" >"$finished"
rm -f "$started"

line="copied ${#summary[@]} databases ($((total / 1048576)) MiB) onto ${TARGET:-the volume}:"
for part in "${summary[@]}"; do
	line+=" $part;"
done
line=${line%;}
say "$line"
printf '%.3000s' "$line" >"$termlog" 2>/dev/null || true
