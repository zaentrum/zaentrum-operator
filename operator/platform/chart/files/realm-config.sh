#!/bin/bash
# Brings what the platform decides about the bundled realm into the realm
# itself. The realm import writes it only when the realm is first made; an
# install whose realm is older still has whatever was imported then — redirect
# URIs that allowed any site, say. This runs as the realm Job
# (templates/realm.yaml), from the same Keycloak image the platform runs, after
# every install and upgrade that changes what it sets.
#
# For each client in REALM_CLIENTS it sets exactly the redirect URIs, web
# origins and post-logout redirect URIs the chart derived from the platform's
# own origins (z.realmClients), and nothing else of the client: every other
# setting and attribute stays as it is. A client that already has them is left
# alone; one the realm lacks is reported, not made — making clients is the
# realm import's job.
#
# Then, last, where the admin console signs in: the master realm's frontend
# URL, MASTER_FRONTEND_URL — the port-forward's address while the console is
# not on the public host, so its sign-in pages stay on the port-forward with
# it, or unset when it is. Last, because Keycloak refuses every master token
# issued before that changes, this run's own included.
#
# Secrets travel in the environment only: kcadm reads the bootstrap admin's
# password from KC_CLI_PASSWORD. Nothing here prints one. On failure the
# reason, and only the reason, is the termination message, which the operator
# reports in the RealmConfigured condition; on success it is the summary.
#
# Environment (set by the Job):
#   KC_SERVER        the in-cluster admin API base, e.g. http://keycloak:80/auth
#   KC_ADMIN_USER    the master realm's bootstrap admin (zaentrum-keycloak-admin)
#   KC_CLI_PASSWORD  its password (read by kcadm itself)
#   REALM_CLIENTS    one line per client:
#                      <clientId>|<redirect URIs>|<web origins>|<post-logout URIs>
#                    the URIs separated by spaces, the post-logout ones by ##
#                    (Keycloak's own separator), each list possibly empty
#   MASTER_FRONTEND_URL  the master realm's frontend URL; empty unsets it.
#                    Not set at all: left as it is.
set -euo pipefail
# The lists are split into words below; a pattern such as zae's
# http://localhost/* must stay a word, never a file name.
set -f

realm=${REALM:-zaentrum}
kcadm=${KCADM:-/opt/keycloak/bin/kcadm.sh}
termlog=${TERMINATION_LOG:-/dev/termination-log}
# Keycloak may still be starting; how long to keep asking it.
wait_seconds=${KC_WAIT_SECONDS:-300}

tmp=$(mktemp -d)
cfg=$tmp/kcadm.config
err=$tmp/err
: >"$err"

fail() {
	printf '%s' "$*" >"$termlog" 2>/dev/null || true
	printf 'realm-config: %s\n' "$*" >&2
	exit 1
}

kc() { "$kcadm" "$@" --config "$cfg" 2>"$err"; }

why() {
	local line
	line=$(tail -n 1 "$err" 2>/dev/null || true)
	printf '%.240s' "${line:-no output}"
}

# js quotes a value as a JSON string.
js() {
	local s=$1
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	s=${s//$'\t'/\\t}
	s=${s//$'\r'/\\r}
	s=${s//$'\n'/\\n}
	printf '"%s"' "$s"
}

# jsarr makes a JSON array of the words it is given.
jsarr() {
	local out="" w
	for w in "$@"; do
		out+="${out:+,}$(js "$w")"
	done
	printf '[%s]' "$out"
}

# sorted splits $1 on the separator $2 and prints the parts sorted, one a line.
sorted() {
	local list=$1 sep=$2
	[ -n "$list" ] || return 0
	printf '%s\n' "${list//$sep/$'\n'}" | sed '/^$/d' | LC_ALL=C sort
}

[ -n "${REALM_CLIENTS:-}" ] ||
	fail "REALM_CLIENTS names no client"
[ -n "${KC_ADMIN_USER:-}" ] && [ -n "${KC_CLI_PASSWORD:-}" ] ||
	fail "Secret zaentrum-keycloak-admin holds no bootstrap admin username and password"

# Keycloak that is still starting is waited for; credentials it refuses are
# not, since asking again would not change its answer.
deadline=$((SECONDS + wait_seconds))
until kc config credentials --server "$KC_SERVER" --realm master --user "$KC_ADMIN_USER" >/dev/null; do
	if [ "$SECONDS" -ge "$deadline" ] || grep -q 'invalid_grant' "$err"; then
		fail "cannot sign in to the Keycloak admin API at $KC_SERVER as the bootstrap admin: $(why)"
	fi
	sleep 5
done

set_clients=() same=() missing=()
while IFS='|' read -r client redirects origins logout; do
	[ -n "$client" ] || continue
	id=$(kc get clients -r "$realm" -q "clientId=$client" --fields id --format csv --noquotes) ||
		fail "cannot look up the client $client in realm $realm: $(why)"
	if [ -z "$id" ]; then
		missing+=("$client")
		continue
	fi

	want="$(sorted "$redirects" ' ')
--
$(sorted "$origins" ' ')
--
$(sorted "$logout" '##')"
	have_redirects=$(kc get "clients/$id" -r "$realm" --fields redirectUris --format csv --noquotes) ||
		fail "cannot read the client $client: $(why)"
	have_origins=$(kc get "clients/$id" -r "$realm" --fields webOrigins --format csv --noquotes) ||
		fail "cannot read the client $client: $(why)"
	have_logout=$(kc get "clients/$id" -r "$realm" --fields 'attributes(post.logout.redirect.uris)' --format csv --noquotes) ||
		fail "cannot read the client $client: $(why)"
	have="$(sorted "$have_redirects" ',')
--
$(sorted "$have_origins" ',')
--
$(sorted "$have_logout" '##')"
	if [ "$want" = "$have" ]; then
		same+=("$client")
		continue
	fi

	# A merge: the lists are replaced, every other setting and attribute of
	# the client is kept. An empty post-logout value removes the attribute.
	# shellcheck disable=SC2086 # the lists are space-separated words
	body="{\"redirectUris\":$(jsarr $redirects),\"webOrigins\":$(jsarr $origins),\"attributes\":{\"post.logout.redirect.uris\":$(js "$logout")}}"
	printf '%s' "$body" | kc update "clients/$id" -r "$realm" -f - -m ||
		fail "cannot set the redirects of the client $client: $(why)"
	echo "realm-config: $client: redirect URIs [${have_redirects}] -> [${redirects// /,}]; web origins [${have_origins}] -> [${origins// /,}]; post-logout [${have_logout}] -> [${logout}]"
	set_clients+=("$client")
done <<<"$REALM_CLIENTS"

console=""
if [ -n "${MASTER_FRONTEND_URL+set}" ]; then
	have=$(kc get realms/master --fields 'attributes(frontendUrl)' --format csv --noquotes) ||
		fail "cannot read the master realm: $(why)"
	where="the admin console signs in on the public host"
	[ -z "$MASTER_FRONTEND_URL" ] || where="the admin console signs in at $MASTER_FRONTEND_URL only"
	if [ "$have" != "$MASTER_FRONTEND_URL" ]; then
		kc update realms/master -s "attributes.frontendUrl=$MASTER_FRONTEND_URL" ||
			fail "cannot set where the admin console signs in: $(why)"
		echo "realm-config: master realm frontend URL [${have}] -> [${MASTER_FRONTEND_URL}]"
		console="now $where"
	else
		console=$where
	fi
fi

summary=""
[ ${#set_clients[@]} -eq 0 ] || summary+="set the redirects of ${set_clients[*]}"
[ ${#same[@]} -eq 0 ] || summary+="${summary:+; }already so: ${same[*]}"
[ ${#missing[@]} -eq 0 ] || summary+="${summary:+; }not in realm $realm: ${missing[*]}"
[ -z "$console" ] || summary+="${summary:+; }$console"
echo "realm-config: $summary"
printf '%s' "$summary" >"$termlog" 2>/dev/null || true
