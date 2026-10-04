#!/usr/bin/env bash
# Makes the Secrets deploy/base generates its Secrets from, with random values:
# deploy/base/secrets/*.env, which git ignores. A file that is there is kept,
# so running it again changes nothing; delete one to make it anew.
#
# deploy/base is UNSUPPORTED (see its kustomization.yaml); this is so that it
# never runs on a password everyone has.
set -euo pipefail

dir="$(cd "$(dirname "$0")" && pwd)/secrets"
mkdir -p "$dir"
umask 077

rand() { LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32; }

make() {
  local file=$dir/$1; shift
  if [ -e "$file" ]; then
    echo "kept $file"
    return
  fi
  printf '%s\n' "$@" >"$file"
  echo "made $file"
}

make db.env "user=zaentrum" "password=$(rand)"
make stream-signing.env "key=$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
make keycloak.env "client-secret=$(rand)"
make keycloak-admin.env "username=admin" "password=$(rand)" "realm-admin-password=$(rand)"
