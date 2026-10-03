#!/bin/bash
# Prepares the account the platform's self-test signs in with, in the bundled
# realm. It runs as the init container of the verification Job (templates/
# tests/verify.yaml), from the same Keycloak image the platform runs, right
# before the checks start.
#
# Every run converges the account instead of trusting what an earlier run left
# behind, so a password someone changed, a lockout or a role someone granted by
# hand heals itself on the next run:
#
#   enabled; email, first and last name set and the email verified, so the
#   realm's profile checks never interrupt a sign-in; no required actions; a
#   NON-temporary password equal to the one in Secret zaentrum-verify; the
#   realm role zaentrum-user and nothing else (the checks only read); no
#   brute-force lockout.
#
# Secrets travel in the environment only, never on a command line: kcadm reads
# the bootstrap admin's password from KC_CLI_PASSWORD, and the account's
# password reaches Keycloak as a JSON body on stdin, written by shell builtins.
# Nothing here prints a secret. On failure the reason, and only the reason, is
# written to the termination message, which the operator reports in
# status.verification.message.
#
# Environment (set by the Job):
#   KC_SERVER         the in-cluster admin API base, e.g. http://keycloak:80/auth
#   KC_ADMIN_USER     the master realm's bootstrap admin (zaentrum-keycloak-admin)
#   KC_CLI_PASSWORD   its password (read by kcadm itself)
#   VERIFY_USERNAME   from Secret zaentrum-verify; must be the account below
#   VERIFY_PASSWORD   from Secret zaentrum-verify
#   VERIFY_EMAIL      the account's email address
set -euo pipefail

# The one account this script may touch. It resets a password and strips roles,
# so a Secret naming somebody else (the realm's admin, a person) is refused
# rather than obeyed.
account=zaentrum-verify
realm=${REALM:-zaentrum}
kcadm=${KCADM:-/opt/keycloak/bin/kcadm.sh}
termlog=${TERMINATION_LOG:-/dev/termination-log}

tmp=$(mktemp -d)
cfg=$tmp/kcadm.config
err=$tmp/err
: >"$err"

fail() {
	printf '%s' "$*" >"$termlog" 2>/dev/null || true
	printf 'verify-account: %s\n' "$*" >&2
	exit 1
}

# kc runs kcadm against the session the sign-in below stored; its stderr is kept
# for why().
kc() { "$kcadm" "$@" --config "$cfg" 2>"$err"; }

# why is the last line kcadm wrote to stderr, where it states its error.
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

[ -n "${VERIFY_PASSWORD:-}" ] ||
	fail "Secret zaentrum-verify holds no password"
[ "${VERIFY_USERNAME:-}" = "$account" ] ||
	fail "Secret zaentrum-verify must name the account $account in the bundled realm, not '${VERIFY_USERNAME:-}'"
[ -n "${KC_ADMIN_USER:-}" ] && [ -n "${KC_CLI_PASSWORD:-}" ] ||
	fail "Secret zaentrum-keycloak-admin holds no bootstrap admin username and password"

kc config credentials --server "$KC_SERVER" --realm master --user "$KC_ADMIN_USER" >/dev/null ||
	fail "cannot sign in to the Keycloak admin API at $KC_SERVER as the bootstrap admin: $(why)"

id=$(kc get users -r "$realm" -q "username=$account" -q exact=true --fields id --format csv --noquotes) ||
	fail "cannot look up the account $account in realm $realm: $(why)"

profile="{\"username\":$(js "$account"),\"enabled\":true,\"emailVerified\":true,\"email\":$(js "${VERIFY_EMAIL:-$account@zaentrum.invalid}"),\"firstName\":\"Zaentrum\",\"lastName\":\"Verification\",\"requiredActions\":[]}"
if [ -z "$id" ]; then
	id=$(printf '%s' "$profile" | kc create users -r "$realm" -f - -i) ||
		fail "cannot create the account $account in realm $realm: $(why)"
	echo "verify-account: created $account in realm $realm"
else
	printf '%s' "$profile" | kc update "users/$id" -r "$realm" -f - ||
		fail "cannot update the account $account: $(why)"
fi

kc delete "attack-detection/brute-force/users/$id" -r "$realm" ||
	fail "cannot clear a sign-in lockout of the account $account: $(why)"

# The password is set only when it does not already sign in. A blind reset on
# every run would trip a realm password-history policy from the second run on;
# probing first converges just the same and writes a credential only when one is
# wrong. The probe signs in through the realm's admin-cli client, the password
# again in KC_CLI_PASSWORD; a probe that fails for any reason means "reset".
probed=fail
if KC_CLI_PASSWORD=$VERIFY_PASSWORD "$kcadm" config credentials --server "$KC_SERVER" --realm "$realm" \
	--user "$account" --client admin-cli --config "$tmp/probe.config" >/dev/null 2>&1; then
	probed=ok
fi
set_password() {
	printf '{"type":"password","temporary":false,"value":%s}' "$(js "$VERIFY_PASSWORD")" |
		kc update "users/$id/reset-password" -r "$realm" -f - -n
}
if [ "$probed" != ok ]; then
	if ! set_password; then
		# A realm policy may refuse the Secret's password because the account
		# had it before (passwordHistory) — after someone changed it by hand,
		# say. A new account has no history, and this one is the operator's
		# own, referred to by nothing; so it is made anew rather than left
		# unable to sign in.
		refused=$(why)
		kc delete "users/$id" -r "$realm" ||
			fail "cannot set the password of the account $account ($refused), nor remove it: $(why)"
		id=$(printf '%s' "$profile" | kc create users -r "$realm" -f - -i) ||
			fail "cannot create the account $account in realm $realm: $(why)"
		set_password ||
			fail "cannot set the password of the account $account: $(why)"
		echo "verify-account: made $account anew; its password could not be set back ($refused)"
	fi
	# The failed probe counted as a failed sign-in; leave no trace of it.
	kc delete "attack-detection/brute-force/users/$id" -r "$realm" ||
		fail "cannot clear a sign-in lockout of the account $account: $(why)"
	echo "verify-account: set the password of $account"
fi

kc add-roles -r "$realm" --uid "$id" --rolename zaentrum-user ||
	fail "cannot grant the account $account the realm role zaentrum-user: $(why)"

# Least privilege: zaentrum-user is the only realm role the account keeps, the
# realm's default-roles composite included, and it holds no client role at all.
roles=$(kc get-roles -r "$realm" --uid "$id" --fields name --format csv --noquotes) ||
	fail "cannot read the realm roles of the account $account: $(why)"
extra=()
while IFS= read -r role; do
	case $role in
	"" | zaentrum-user) ;;
	*) extra+=(--rolename "$role") ;;
	esac
done <<<"$roles"
if [ ${#extra[@]} -gt 0 ]; then
	kc remove-roles -r "$realm" --uid "$id" "${extra[@]}" ||
		fail "cannot remove the extra realm roles of the account $account: $(why)"
fi

mappings=$(kc get "users/$id/role-mappings" -r "$realm") ||
	fail "cannot read the role mappings of the account $account: $(why)"
clients=$(printf '%s\n' "$mappings" | grep -o '"client" *: *"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/' || true)
while IFS= read -r client; do
	[ -n "$client" ] || continue
	names=$(kc get-roles -r "$realm" --uid "$id" --cclientid "$client" --fields name --format csv --noquotes) ||
		fail "cannot read the $client roles of the account $account: $(why)"
	drop=()
	while IFS= read -r role; do
		[ -n "$role" ] && drop+=(--rolename "$role")
	done <<<"$names"
	if [ ${#drop[@]} -gt 0 ]; then
		kc remove-roles -r "$realm" --uid "$id" --cclientid "$client" "${drop[@]}" ||
			fail "cannot remove the $client roles of the account $account: $(why)"
	fi
done <<<"$clients"

echo "verify-account: $account is ready in realm $realm"
