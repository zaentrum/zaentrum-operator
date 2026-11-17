#!/bin/bash
# Measures one claim of the platform, read-only: the filesystem it is on, with
# its size, what an ordinary writer can still use of it, and its id. It runs as
# a measuring Job (templates/storage.yaml), from the image the Postgres runs;
# the operator starts the Jobs and reads the report this leaves as the
# termination message into status.storage and status.backup.sets
# (internal/controller/storage.go).
#
# The library's Job also asks the bundled Postgres for the size of each
# platform database and, where the Postgres keeps its data on a claim, for the
# size, the free space and the id of the filesystem its data is on: the server
# reads those itself, for its superuser, running stat (COPY ... FROM PROGRAM).
# The databases' names reach psql as values it quotes, each whole. A server
# that may not run a program for its superuser (SQLSTATE 42501) is no failure:
# the report says why the volume of the data is not measured.
#
# The backups' Job also lists the sets on the claim, newest first, each with
# its size, the version in its backup.json, whether this platform made it (the
# uid in its backup.json is the one PLATFORM names) and whether its SHA256SUMS
# is there. backup.json is read for its first field alone, platform: a version
# or a uid further on, as the resource's or an addon's, is never taken for the
# platform's. A backup.json that cannot be read, or names a version of more
# than 128 characters, gives no version. The newest 30, fewer where 30 do not
# fit the report, and the one SET names, however old.
#
# The report is one line of JSON, sizes in bytes, never more than REPORT_BYTES
# (4096: what the kubelet keeps of a termination message):
#   {"v":1,"size":S,"free":F,"fs":"ID","databases":{"KEY":B,...},
#    "postgres":{"size":S,"free":F,"fs":"ID"} or {"refused":"ERROR LINE"},
#    "total":N,"sets":[["NAME",B,"VERSION",WHOLE,OWN],...],
#    "set":["NAME",B,"VERSION",WHOLE,OWN],"failed":"REASON"}
# databases by their key (chino, katalog, keycloak, portal); total is how many
# sets the claim holds, sets the newest listed, and set the one SET names where
# it is not among them. "failed" is there where something could not be
# measured, and then the exit code is 1; what was measured is in the report
# either way. OWN is null where nothing says who made the set.
#
# Secrets travel in the environment only (PGPASSWORD, from Secret zaentrum-db).
#
# Environment (set by the Job):
#   MEASURE_DIR                  where the claim is mounted, read-only
#   PGHOST, PGUSER, PGPASSWORD   the bundled Postgres and its superuser
#   DB_CHINO, DB_KATALOG,        the platform's databases, each by its key
#   DB_KEYCLOAK, DB_PORTAL
#   POSTGRES_VOLUME              "true": the Postgres keeps its data on a claim
#   SETS                         "true": list the sets on the claim
#   SET                          a set's name, reported however old
#   PLATFORM                     the platform of the ConfigMap zaentrum-backup
set -Eeuo pipefail
export LC_ALL=C

termlog=${TERMINATION_LOG:-/dev/termination-log}
wait_seconds=${PG_WAIT_SECONDS:-120}
budget=${REPORT_BYTES:-4096}
dir=${MEASURE_DIR:-/measure}
newest=30
longest_version=128
report="" failed=""

say() { printf 'measure: %s\n' "$*"; }

# js quotes a value as a JSON string.
js() {
	local s=$1
	s=${s//[[:cntrl:]]/}
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	printf '"%s"' "$s"
}

# fail keeps why something was not measured: the first reason is the report's.
fail() {
	[ -n "$failed" ] || failed=$*
	printf 'measure: %s\n' "$*" >&2
}

# finish writes the report and ends: 1 where something was not measured. A
# reason is cut where the report would grow past what the kubelet keeps.
finish() {
	trap - ERR
	local out="{\"v\":1$report" why=$failed room
	if [ -n "$why" ]; then
		room=$(((budget - ${#out} - 14) / 2))
		[ "$room" -gt 0 ] || room=0
		[ "${#why}" -le "$room" ] || why=${why:0:$room}
		out="$out,\"failed\":$(js "$why")"
	fi
	printf '%s}' "$out" >"$termlog" 2>/dev/null || true
	[ -z "$failed" ] || exit 1
	exit 0
}
trap '[ "$BASH_SUBSHELL" = 0 ] || exit 1; fail "the measuring stopped at line $LINENO of measure-volume.sh; the Job log says why"; finish' ERR

# figures turns a line of stat -f, "BLOCK-SIZE BLOCKS BLOCKS-AN-ORDINARY-
# WRITER-CAN-USE ID", into "SIZE FREE ID", in bytes.
figures() {
	local bs blocks avail id n
	read -r bs blocks avail id _ <<<"$1"
	for n in "$bs" "$blocks" "$avail"; do
		case $n in '' | *[!0-9]*) return 1 ;; esac
	done
	[ -n "$id" ] || return 1
	printf '%s %s %s' "$((bs * blocks))" "$((bs * avail))" "$id"
}

q() { psql -X -q -v ON_ERROR_STOP=1 -At -d postgres "$@"; }

# psql_error is the code and the line of what psql said of a failure, "CODE
# LINE" (verbose: ERROR:  CODE: LINE), else its last line alone.
psql_error() {
	local e
	e=$(printf '%s\n' "$1" | awk '/ERROR:/ { sub(/^.*ERROR:[ \t]*/, ""); print; exit }')
	if [ -z "$e" ]; then
		printf ' %s' "$(printf '%s\n' "$1" | awk 'NF { last = $0 } END { print last }')"
		return 0
	fi
	case $e in
	[0-9A-Z][0-9A-Z][0-9A-Z][0-9A-Z][0-9A-Z]:\ *) printf '%s %s' "${e%%:*}" "${e#*: }" ;;
	*) printf ' %s' "$e" ;;
	esac
}

# measure_postgres asks the bundled Postgres for the size of each platform
# database and, with POSTGRES_VOLUME, for the filesystem its data is on.
measure_postgres() {
	if [ -z "${PGUSER:-}" ] || [ -z "${PGPASSWORD:-}" ]; then
		fail "Secret zaentrum-db holds no user and password"
		return 0
	fi
	local deadline=$((SECONDS + wait_seconds))
	until pg_isready -q; do
		if [ "$SECONDS" -ge "$deadline" ]; then
			fail "the Postgres at ${PGHOST:-localhost} does not answer"
			return 0
		fi
		sleep 2
	done
	# The names are values, which psql quotes (:'name'); never text of the
	# statement, which psql reads from its input to put them in.
	local out line key k v n sizes="" total=0
	local sizes_sql="SELECT d.k || ' ' || coalesce(pg_database_size(p.datname)::text, '')
  FROM (VALUES (1, 'chino', :'chino'), (2, 'katalog', :'katalog'), (3, 'keycloak', :'keycloak'), (4, 'portal', :'portal'))
       AS d(i, k, n)
  LEFT JOIN pg_database p ON p.datname = d.n
 ORDER BY d.i;"
	out=$(printf '%s\n' "$sizes_sql" |
		q -v "chino=${DB_CHINO:-}" -v "katalog=${DB_KATALOG:-}" -v "keycloak=${DB_KEYCLOAK:-}" -v "portal=${DB_PORTAL:-}")
	for key in chino katalog keycloak portal; do
		n=""
		while IFS=' ' read -r k v; do
			[ "$k" != "$key" ] || n=$v
		done <<<"$out"
		case $n in '' | *[!0-9]*)
			case $key in
			chino) fail "the Postgres has no database ${DB_CHINO:-}" ;;
			katalog) fail "the Postgres has no database ${DB_KATALOG:-}" ;;
			keycloak) fail "the Postgres has no database ${DB_KEYCLOAK:-}" ;;
			portal) fail "the Postgres has no database ${DB_PORTAL:-}" ;;
			esac
			return 0
			;;
		esac
		sizes="$sizes${sizes:+,}\"$key\":$n"
		total=$((total + n))
	done
	report="$report,\"databases\":{$sizes}"
	say "the databases take $total bytes"
	[ "${POSTGRES_VOLUME:-}" = true ] || return 0

	# The server reads the filesystem its data is on itself, for its
	# superuser: stat, run by the server.
	local pgdir err code
	pgdir=$(q -c "SELECT current_setting('data_directory')")
	if ! out=$(q -v VERBOSITY=verbose -c "CREATE TEMP TABLE measure (line text)" \
		-c "DO \$\$ BEGIN EXECUTE format('COPY pg_temp.measure FROM PROGRAM %L', 'stat -f -c ''%S %b %a %i'' ''' || current_setting('data_directory') || ''''); END \$\$" \
		-c "SELECT line FROM pg_temp.measure" 2>&1); then
		err=$(psql_error "$out")
		code=${err%% *} line=${err#* }
		if [ "$code" = 42501 ]; then
			# The server may not run a program: no failure, and the report
			# says so.
			report="$report,\"postgres\":{\"refused\":$(js "$line")}"
			return 0
		fi
		fail "cannot read the filesystem at $pgdir"
		return 0
	fi
	local fs size free id
	if ! fs=$(figures "$out"); then
		fail "cannot read the filesystem at $pgdir"
		return 0
	fi
	read -r size free id <<<"$fs"
	report="$report,\"postgres\":{\"size\":$size,\"free\":$free,\"fs\":$(js "$id")}"
	say "$pgdir: $size bytes, $free free, filesystem $id"
}

# field prints the value of a string field of a flat JSON object, or nothing.
field() {
	printf '%s' "$2" | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p"
}

# record prints the platform of a set's backup.json, its first field, or
# nothing: nothing further on in the file is read for it. It fails where the
# file cannot be read.
record() {
	[ -r "$1" ] || return 1
	head -c 4096 "$1" | tr -d '\n\r' |
		sed -n 's/^[[:space:]]*{[[:space:]]*"platform"[[:space:]]*:[[:space:]]*{\([^{}]*\)}.*$/\1/p'
}

mine=""

# entry prints a set's line of the report: ["NAME",BYTES,"VERSION",WHOLE,OWN].
entry() {
	local at="$dir/$1" kib whole=false own=null version="" p uid
	kib=$({ du -sk "$at" 2>/dev/null || true; } | cut -f1)
	case $kib in '' | *[!0-9]*) kib=0 ;; esac
	[ ! -f "$at/SHA256SUMS" ] || whole=true
	if [ -f "$at/backup.json" ]; then
		if p=$(record "$at/backup.json"); then
			version=$(field version "$p")
			uid=$(field uid "$p")
			if [ "${#version}" -gt "$longest_version" ]; then
				say "$1: its backup.json names a version of more than $longest_version characters, so it is listed without a version" >&2
				version=""
			fi
			if [ -n "$uid" ] && [ -n "$mine" ]; then
				if [ "$uid" = "$mine" ]; then own=true; else own=false; fi
			fi
		else
			say "$1: its backup.json cannot be read, so it is listed without a version" >&2
		fi
	fi
	printf '[%s,%s,%s,%s,%s]' "$(js "$1")" "$((kib * 1024))" "$(js "$version")" "$whole" "$own"
}

# list_sets lists the sets on the claim, newest first, as the report has
# room for them, and the one SET names: the whole report, that set included,
# holds REPORT_BYTES at most.
list_sets() {
	local names total=0 listed=0 extra="" sets="" e name room
	[ -z "${PLATFORM:-}" ] || mine=$(field uid "$PLATFORM")
	names=$(cd "$dir" && find . -mindepth 1 -maxdepth 1 -type d \
		-name '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]-[0-9][0-9]-[0-9][0-9]Z' |
		sed 's|^\./||' | sort -r)
	[ -z "$names" ] || total=$(printf '%s\n' "$names" | wc -l | tr -d ' ')
	# What the report holds besides the sets, and room for a reason.
	room=$((budget - 6 - ${#report} - ${#total} - 27 - 160))
	if [ -n "${SET:-}" ] && grep -qxF -- "$SET" <<<"$names"; then
		extra=$(entry "$SET")
		if [ $((${#extra} + 7)) -le "$room" ]; then
			room=$((room - ${#extra} - 7))
		else
			extra=""
		fi
	fi
	while IFS= read -r name; do
		[ -n "$name" ] || continue
		[ "$listed" -lt "$newest" ] || break
		if [ -n "$extra" ] && [ "$name" = "${SET:-}" ]; then
			e=$extra extra=""
			room=$((room + ${#e} + 7))
		else
			e=$(entry "$name")
		fi
		[ $((${#sets} + ${#e} + 1)) -le "$room" ] || break
		sets="$sets${sets:+,}$e"
		listed=$((listed + 1))
	done <<<"$names"
	report="$report,\"total\":$total,\"sets\":[$sets]"
	[ -z "$extra" ] || report="$report,\"set\":$extra"
	say "$total sets, the newest $listed reported"
}

if [ ! -d "$dir" ] || [ ! -r "$dir" ] || [ ! -x "$dir" ]; then
	fail "$dir is not a directory this measuring can read"
	finish
fi
if ! line=$(stat -f -c '%S %b %a %i' "$dir" 2>/dev/null) || ! fs=$(figures "$line"); then
	fail "cannot read the filesystem at $dir"
	finish
fi
read -r size free id <<<"$fs"
report="$report,\"size\":$size,\"free\":$free,\"fs\":$(js "$id")"
say "$dir: $size bytes, $free free, filesystem $id"

[ -z "${PGHOST:-}" ] || measure_postgres
[ "${SETS:-}" != true ] || list_sets
finish
