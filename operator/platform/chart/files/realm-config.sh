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
# own origins (z.realmClients), and how the client signs people in as the realm
# import has it: a public client, the flows it allows — the TV apps' client the
# device grant, the phone apps' the authorization code — and none other, PKCE
# on them (z.realmSettings). Nothing else of the client: every other setting
# and attribute stays as it is. A client that already has them is left alone.
# One the realm lacks — a realm imported before the chart had it, or one
# deleted since — is made as the import makes it (REALM_CLIENT_JSON).
#
# Then the client portal-api manages the realm's people with (PEOPLE_CLIENT):
# made as the import makes it when the realm lacks it, signing in as the import
# says (PEOPLE_SETTINGS: confidential, the client credentials grant and no
# other), with the secret in Secret zaentrum-people when there is one, and its
# service account holding exactly the realm-management roles PEOPLE_ROLES
# (view-users, query-users, manage-users) — no other client role, no realm
# role but the realm's default roles. A role someone granted it by hand goes.
#
# Then a person's rating cap: the user attribute max_rating, declared in the
# realm's user profile as PROFILE_ATTRIBUTE says — Keycloak keeps no attribute
# its profile does not declare — so that only an admin sees or changes it; and
# on each client of RATING_CLIENTS the protocol mapper RATING_MAPPER_JSON that
# puts it into the access token as the claim max_rating. A mapper of that name
# whose config is not RATING_MAPPER_CONFIG is made anew. The realm stops asking
# people at sign-in to complete their profile (the required action
# VERIFY_PROFILE): a person here may have no email, and a child no last name.
# And a realm without a password policy gets PASSWORD_POLICY.
#
# Then the demo user. A realm imported without Secret zaentrum-demo-user gave it
# the password "${DEMO_USER_PASSWORD}" — Keycloak leaves a placeholder it cannot
# resolve as it is — the same on every such install. If it still signs in with
# that, it gets the Secret's password, or, without one, is disabled. A demo
# user with a password of its own is left alone; the probe costs it one failed
# sign-in.
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
#                      <clientId>|<redirect URIs>|<web origins>|<post-logout URIs>|<settings>
#                    the URIs separated by spaces, the post-logout ones by ##
#                    (Keycloak's own separator), each list possibly empty; the
#                    settings space-separated name=value, a client field or,
#                    with a dot in its name, an attribute
#   REALM_CLIENT_JSON  one line per client: <clientId> <its representation, as
#                    the realm import has it>, for a client the realm lacks
#   PEOPLE_CLIENT    the people client's clientId; unset: no such client
#   PEOPLE_CLIENT_JSON  its representation, as the realm import has it
#   PEOPLE_SETTINGS  how it signs in, as REALM_CLIENTS' settings
#   PEOPLE_ROLES     its service account's realm-management roles, by name
#   PEOPLE_CLIENT_SECRET  from Secret zaentrum-people, when there is one
#   RATING_CLIENTS   the clients whose access tokens carry the rating cap
#   RATING_MAPPER_JSON    the protocol mapper that puts it there
#   RATING_MAPPER_CONFIG  its config, space-separated name=value
#   PROFILE_ATTRIBUTE     the user attribute max_rating, as the realm's user
#                    profile declares it
#   PROFILE_ATTRIBUTE_CHECK  the part of it that must be so: a projection of
#                    the profile's attributes, as Keycloak answers it
#   PASSWORD_POLICY  the password policy of a realm that has none
#   MASTER_FRONTEND_URL  the master realm's frontend URL; empty unsets it.
#                    Not set at all: left as it is.
#   DEMO_USER_PASSWORD   from Secret zaentrum-demo-user, when there is one
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

# keep_settings makes the client $1 (id $2) sign people in as the settings $3
# say: space-separated name=value, a client field or, with a dot in its name,
# an attribute. A field reads as true or false; an attribute the client does
# not have reads as empty, which for a switch means off. What differs is set
# with one merge — what is named changes, nothing else — and said in
# $changed, which is empty when the client was so already.
keep_settings() {
	local client=$1 id=$2 settings=$3 setting name value have fields="" attrs="" body
	changed=""
	for setting in $settings; do
		name=${setting%%=*} value=${setting#*=}
		case $name in
		*.*) have=$(kc get "clients/$id" -r "$realm" --fields "attributes($name)" --format csv --noquotes) ;;
		*) have=$(kc get "clients/$id" -r "$realm" --fields "$name" --format csv --noquotes) ;;
		esac || fail "cannot read the client $client: $(why)"
		if [ "$have" = "$value" ] || { [ -z "$have" ] && [ "$value" = false ]; }; then
			continue
		fi
		case $name in
		*.*) attrs+="${attrs:+,}$(js "$name"):$(js "$value")" ;;
		*) fields+="${fields:+,}$(js "$name"):$value" ;;
		esac
		changed+="${changed:+; }$name ${have:-unset} -> $value"
	done
	if [ -n "$changed" ]; then
		body="{${fields}${fields:+${attrs:+,}}${attrs:+\"attributes\":{$attrs}}}"
		printf '%s' "$body" | kc update "clients/$id" -r "$realm" -f - -m ||
			fail "cannot set how the client $client signs in: $(why)"
	fi
}

# client_id prints the id of the client $1 in the realm, or nothing when the
# realm has none of that clientId.
client_id() {
	kc get clients -r "$realm" -q "clientId=$1" --fields id --format csv --noquotes ||
		fail "cannot look up the client $1 in realm $realm: $(why)"
}

# imported prints the realm import's representation of a client, or fails.
imported() {
	local want=$1 name rep
	while read -r name rep; do
		if [ "$name" = "$want" ] && [ -n "$rep" ]; then
			printf '%s' "$rep"
			return 0
		fi
	done <<<"${REALM_CLIENT_JSON:-}"
	return 1
}

set_clients=() set_flows=() made=() same=() missing=()
while IFS='|' read -r client redirects origins logout settings; do
	[ -n "$client" ] || continue
	id=$(kc get clients -r "$realm" -q "clientId=$client" --fields id --format csv --noquotes) ||
		fail "cannot look up the client $client in realm $realm: $(why)"
	if [ -z "$id" ]; then
		# The import's representation carries the redirects and settings
		# above already: made so, it is in step.
		if ! rep=$(imported "$client"); then
			missing+=("$client")
			continue
		fi
		printf '%s' "$rep" | kc create clients -r "$realm" -f - >/dev/null ||
			fail "cannot make the client $client: $(why)"
		echo "realm-config: $client: made, as the realm import makes it"
		made+=("$client")
		continue
	fi

	# How the client signs people in.
	keep_settings "$client" "$id" "$settings"
	flows=$changed
	if [ -n "$flows" ]; then
		echo "realm-config: $client: $flows"
		set_flows+=("$client")
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
		[ -n "$flows" ] || same+=("$client")
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

# The people client: portal-api manages the realm's people with it, and with
# nothing more than that.
people=() people_secret=""
if [ -n "${PEOPLE_CLIENT:-}" ]; then
	client=$PEOPLE_CLIENT
	id=$(client_id "$client")
	if [ -z "$id" ]; then
		[ -n "${PEOPLE_CLIENT_JSON:-}" ] || fail "PEOPLE_CLIENT_JSON holds no representation of the client $client"
		printf '%s' "$PEOPLE_CLIENT_JSON" | kc create clients -r "$realm" -f - >/dev/null ||
			fail "cannot make the client $client: $(why)"
		id=$(client_id "$client")
		[ -n "$id" ] || fail "the client $client was made, and cannot be found"
		echo "realm-config: $client: made, as the realm import makes it"
		people+=("made")
	fi

	keep_settings "$client" "$id" "${PEOPLE_SETTINGS:-}"
	if [ -n "$changed" ]; then
		echo "realm-config: $client: $changed"
		people+=("set how it signs in")
	fi

	# The secret portal-api signs in with. Read and compared here, never
	# printed; it reaches Keycloak as a JSON body on stdin, written by shell
	# builtins. Without the Secret the client keeps a secret nobody holds,
	# and the People page answers that it is not set up.
	if [ -n "${PEOPLE_CLIENT_SECRET:-}" ]; then
		have=$(kc get "clients/$id/client-secret" -r "$realm" --fields value --format csv --noquotes) ||
			fail "cannot read the secret of the client $client: $(why)"
		if [ "$have" != "$PEOPLE_CLIENT_SECRET" ]; then
			printf '{"secret":%s}' "$(js "$PEOPLE_CLIENT_SECRET")" | kc update "clients/$id" -r "$realm" -f - -m ||
				fail "cannot set the secret of the client $client: $(why)"
			echo "realm-config: $client: its secret is the one in Secret zaentrum-people now"
			people+=("set its secret")
		fi
		have=""
	else
		people_secret="no Secret zaentrum-people holds the secret of $client"
		echo "realm-config: $client: $people_secret"
	fi

	# Its service account: PEOPLE_ROLES of realm-management, no other client
	# role, and no realm role but the realm's default roles.
	sa=$(kc get "clients/$id/service-account-user" -r "$realm" --fields id --format csv --noquotes) ||
		fail "cannot read the service account of the client $client: $(why)"
	[ -n "$sa" ] || fail "the client $client has no service account"
	granted=$(kc get-roles -r "$realm" --uid "$sa" --cclientid realm-management --fields name --format csv --noquotes) ||
		fail "cannot read the realm-management roles of the client $client: $(why)"
	add=() drop=() roles_changed=""
	for role in ${PEOPLE_ROLES:-}; do
		if ! grep -qxF -- "$role" <<<"$granted"; then
			add+=(--rolename "$role")
			roles_changed+="${roles_changed:+, }+$role"
		fi
	done
	while IFS= read -r role; do
		[ -n "$role" ] || continue
		case " ${PEOPLE_ROLES:-} " in
		*" $role "*) ;;
		*)
			drop+=(--rolename "$role")
			roles_changed+="${roles_changed:+, }-$role"
			;;
		esac
	done <<<"$granted"
	if [ ${#add[@]} -gt 0 ]; then
		kc add-roles -r "$realm" --uid "$sa" --cclientid realm-management "${add[@]}" ||
			fail "cannot grant the client $client its realm-management roles: $(why)"
	fi
	if [ ${#drop[@]} -gt 0 ]; then
		kc remove-roles -r "$realm" --uid "$sa" --cclientid realm-management "${drop[@]}" ||
			fail "cannot remove the extra realm-management roles of the client $client: $(why)"
	fi
	realm_roles=$(kc get-roles -r "$realm" --uid "$sa" --fields name --format csv --noquotes) ||
		fail "cannot read the realm roles of the client $client: $(why)"
	drop=()
	while IFS= read -r role; do
		case $role in
		"" | "default-roles-$realm") ;;
		*)
			drop+=(--rolename "$role")
			roles_changed+="${roles_changed:+, }-$role"
			;;
		esac
	done <<<"$realm_roles"
	if [ ${#drop[@]} -gt 0 ]; then
		kc remove-roles -r "$realm" --uid "$sa" "${drop[@]}" ||
			fail "cannot remove the realm roles of the client $client: $(why)"
	fi
	mappings=$(kc get "users/$sa/role-mappings" -r "$realm") ||
		fail "cannot read the role mappings of the client $client: $(why)"
	others=$(printf '%s\n' "$mappings" | grep -o '"client" *: *"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/' || true)
	while IFS= read -r other; do
		[ -n "$other" ] && [ "$other" != realm-management ] || continue
		names=$(kc get-roles -r "$realm" --uid "$sa" --cclientid "$other" --fields name --format csv --noquotes) ||
			fail "cannot read the $other roles of the client $client: $(why)"
		drop=()
		while IFS= read -r role; do
			if [ -n "$role" ]; then
				drop+=(--rolename "$role")
				roles_changed+="${roles_changed:+, }-$other/$role"
			fi
		done <<<"$names"
		if [ ${#drop[@]} -gt 0 ]; then
			kc remove-roles -r "$realm" --uid "$sa" --cclientid "$other" "${drop[@]}" ||
				fail "cannot remove the $other roles of the client $client: $(why)"
		fi
	done <<<"$others"
	if [ -n "$roles_changed" ]; then
		echo "realm-config: $client: its service account's roles: $roles_changed"
		people+=("set its roles ($roles_changed)")
	fi
fi

# A person's rating cap. First the user attribute: Keycloak keeps no attribute
# the realm's user profile does not declare, and one a person could change
# would cap nothing.
profile=""
if [ -n "${PROFILE_ATTRIBUTE:-}" ]; then
	[[ $PROFILE_ATTRIBUTE =~ \"name\":\"([^\"]+)\" ]] ||
		fail "PROFILE_ATTRIBUTE names no attribute"
	attribute=${BASH_REMATCH[1]}
	have=$(kc get users/profile -r "$realm" \
		--fields 'attributes(name,validations(integer(min,max)),required(roles,scopes),permissions(view,edit),multivalued)') ||
		fail "cannot read the user profile of realm $realm: $(why)"
	have=$(printf '%s' "$have" | tr -d '\n' | sed 's/ //g')
	if [[ $have != *"$PROFILE_ATTRIBUTE_CHECK"* ]]; then
		names=$(kc get users/profile -r "$realm" --fields 'attributes(name)' --format csv --noquotes) ||
			fail "cannot read the user profile of realm $realm: $(why)"
		at=-1 i=0
		IFS=, read -ra listed <<<"$names"
		for name in "${listed[@]}"; do
			[ "$name" != "$attribute" ] || at=$i
			i=$((i + 1))
		done
		if [ "$at" -lt 0 ]; then
			kc update users/profile -r "$realm" -s "attributes+=$PROFILE_ATTRIBUTE" ||
				fail "cannot declare the user attribute $attribute: $(why)"
			profile="declared the user attribute $attribute"
		else
			kc update users/profile -r "$realm" -s "attributes[$at]=$PROFILE_ATTRIBUTE" ||
				fail "cannot set the user attribute $attribute: $(why)"
			profile="set the user attribute $attribute as declared"
		fi
		echo "realm-config: $profile"
	fi
fi

# Then the mapper on each client people watch through. A client the realm
# lacks was made above, mapper and all, or is not the import's.
rated=() unrated=()
if [ -n "${RATING_MAPPER_JSON:-}" ]; then
	[[ $RATING_MAPPER_JSON =~ \"name\":\"([^\"]+)\" ]] ||
		fail "RATING_MAPPER_JSON names no mapper"
	mapper=${BASH_REMATCH[1]}
	[[ $RATING_MAPPER_JSON =~ \"protocolMapper\":\"([^\"]+)\" ]] ||
		fail "RATING_MAPPER_JSON names no kind of mapper"
	kind=${BASH_REMATCH[1]}
	for client in ${RATING_CLIENTS:-}; do
		id=$(client_id "$client")
		if [ -z "$id" ]; then
			unrated+=("$client")
			continue
		fi
		models=$(kc get "clients/$id/protocol-mappers/models" -r "$realm" --fields id,name --format csv --noquotes) ||
			fail "cannot read the protocol mappers of the client $client: $(why)"
		mid=$(printf '%s\n' "$models" | sed -n "s/^\([^,]*\),$mapper\$/\1/p" | head -n 1)
		if [ -n "$mid" ]; then
			have=$(kc get "clients/$id/protocol-mappers/models/$mid" -r "$realm") ||
				fail "cannot read the mapper $mapper of the client $client: $(why)"
			as_wanted=true
			grep -qF "\"protocolMapper\" : \"$kind\"" <<<"$have" || as_wanted=false
			for pair in ${RATING_MAPPER_CONFIG:-}; do
				grep -qF "\"${pair%%=*}\" : \"${pair#*=}\"" <<<"$have" || as_wanted=false
			done
			if $as_wanted; then
				continue
			fi
			kc delete "clients/$id/protocol-mappers/models/$mid" -r "$realm" ||
				fail "cannot remove the mapper $mapper of the client $client: $(why)"
		fi
		printf '%s' "$RATING_MAPPER_JSON" | kc create "clients/$id/protocol-mappers/models" -r "$realm" -f - >/dev/null ||
			fail "cannot give the client $client the mapper $mapper: $(why)"
		echo "realm-config: $client: the mapper $mapper puts the rating cap into its access tokens"
		rated+=("$client")
	done
fi

# A person here may have no email and a child no last name: the realm does
# not stop them at sign-in to ask for either.
verify=""
have=$(kc get authentication/required-actions/VERIFY_PROFILE -r "$realm" --fields enabled --format csv --noquotes) ||
	fail "cannot read the required action VERIFY_PROFILE of realm $realm: $(why)"
if [ "$have" = true ]; then
	kc update authentication/required-actions/VERIFY_PROFILE -r "$realm" -s enabled=false ||
		fail "cannot turn off the required action VERIFY_PROFILE: $(why)"
	verify="sign-in no longer asks to complete a profile (VERIFY_PROFILE off)"
	echo "realm-config: $verify"
fi

# A realm without a password policy gets the platform's.
policy=""
if [ -n "${PASSWORD_POLICY:-}" ]; then
	have=$(kc get "realms/$realm" --fields passwordPolicy --format csv --noquotes) ||
		fail "cannot read realm $realm: $(why)"
	if [ -z "$have" ]; then
		kc update "realms/$realm" -s "passwordPolicy=$PASSWORD_POLICY" ||
			fail "cannot set the password policy of realm $realm: $(why)"
		policy="the password policy is $PASSWORD_POLICY"
		echo "realm-config: $policy"
	fi
fi

demo=""
placeholder='${DEMO_USER_PASSWORD}'
demo_id=$(kc get users -r "$realm" -q username=demo -q exact=true --fields id --format csv --noquotes) ||
	fail "cannot look up the demo user in realm $realm: $(why)"
if [ -n "$demo_id" ] && KC_CLI_PASSWORD=$placeholder "$kcadm" config credentials --server "$KC_SERVER" --realm "$realm" \
	--user demo --client admin-cli --config "$tmp/probe.config" >/dev/null 2>&1; then
	if [ -n "${DEMO_USER_PASSWORD:-}" ] && [ "$DEMO_USER_PASSWORD" != "$placeholder" ]; then
		printf '{"type":"password","temporary":false,"value":%s}' "$(js "$DEMO_USER_PASSWORD")" |
			kc update "users/$demo_id/reset-password" -r "$realm" -f - -n ||
			fail "cannot set the password of the demo user: $(why)"
		demo="the demo user no longer signs in with the placeholder password: it has the one in Secret zaentrum-demo-user"
	else
		kc update "users/$demo_id" -r "$realm" -s enabled=false ||
			fail "cannot disable the demo user: $(why)"
		demo="the demo user signed in with the placeholder password and no Secret zaentrum-demo-user gives it another: disabled"
	fi
	echo "realm-config: $demo"
fi

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
[ ${#made[@]} -eq 0 ] || summary+="made ${made[*]}"
[ ${#set_clients[@]} -eq 0 ] || summary+="${summary:+; }set the redirects of ${set_clients[*]}"
[ ${#set_flows[@]} -eq 0 ] || summary+="${summary:+; }set the sign-in flows of ${set_flows[*]}"
[ ${#same[@]} -eq 0 ] || summary+="${summary:+; }already so: ${same[*]}"
[ ${#missing[@]} -eq 0 ] || summary+="${summary:+; }not in realm $realm: ${missing[*]}"
if [ ${#people[@]} -gt 0 ]; then
	list=$(printf '%s, ' "${people[@]}")
	summary+="${summary:+; }${PEOPLE_CLIENT}: ${list%, }"
fi
[ -z "$people_secret" ] || summary+="${summary:+; }$people_secret"
[ -z "$profile" ] || summary+="${summary:+; }$profile"
[ ${#rated[@]} -eq 0 ] || summary+="${summary:+; }mapped the rating cap for ${rated[*]}"
[ ${#unrated[@]} -eq 0 ] || summary+="${summary:+; }no client to map the rating cap for: ${unrated[*]}"
[ -z "$verify" ] || summary+="${summary:+; }$verify"
[ -z "$policy" ] || summary+="${summary:+; }$policy"
[ -z "$demo" ] || summary+="${summary:+; }$demo"
[ -z "$console" ] || summary+="${summary:+; }$console"
echo "realm-config: $summary"
printf '%s' "$summary" >"$termlog" 2>/dev/null || true
