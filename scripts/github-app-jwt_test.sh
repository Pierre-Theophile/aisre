#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The GitHub App JWT helper's own test. It signs with a throwaway key, then checks what GitHub
# checks: three parts, an RS256 header, `iss` as given, a lifetime under ten minutes with `iat` in
# the past, and a signature the matching public key verifies. Then it checks that each missing input
# is refused by name and that nothing but the token reaches stdout.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
helper="$here/github-app-jwt.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
b64url_decode() {
	local s="${1//-/+}"
	s="${s//_//}"
	while (( ${#s} % 4 )); do s+="="; done
	printf '%s' "$s" | openssl base64 -d -A
}

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work/key.pem" 2>/dev/null
openssl pkey -in "$work/key.pem" -pubout -out "$work/pub.pem"

token=$(GITHUB_APP_ID=Iv23test GITHUB_APP_PRIVATE_KEY="$(cat "$work/key.pem")" "$helper")
IFS=. read -r h p s <<<"$token"
[[ -n "$h" && -n "$p" && -n "$s" ]] || fail "expected three dot-separated parts, got: $token"
[[ "$token" == "$h.$p.$s" ]] || fail "expected exactly three parts"

[[ "$(b64url_decode "$h")" == '{"alg":"RS256","typ":"JWT"}' ]] || fail "header: $(b64url_decode "$h")"
claims=$(b64url_decode "$p")
[[ "$claims" == *'"iss":"Iv23test"'* ]] || fail "iss not carried: $claims"
iat=$(sed -E 's/.*"iat":([0-9]+).*/\1/' <<<"$claims")
exp=$(sed -E 's/.*"exp":([0-9]+).*/\1/' <<<"$claims")
now=$(date +%s)
(( iat < now )) || fail "iat $iat is not in the past (now $now)"
(( exp - iat <= 600 )) || fail "lifetime $((exp - iat))s exceeds GitHub's ten minutes"
(( exp > now )) || fail "exp $exp is already past"

b64url_decode "$s" >"$work/sig"
printf '%s.%s' "$h" "$p" | openssl dgst -sha256 -verify "$work/pub.pem" -signature "$work/sig" >/dev/null ||
	fail "signature does not verify against the public key"

for missing in GITHUB_APP_ID GITHUB_APP_PRIVATE_KEY; do
	set +e
	out=$(env -u "$missing" GITHUB_APP_ID=x GITHUB_APP_PRIVATE_KEY="$(cat "$work/key.pem")" \
		bash -c "unset $missing; '$helper'" 2>"$work/err")
	code=$?
	set -e
	(( code != 0 )) || fail "a missing $missing was not refused"
	[[ -z "$out" ]] || fail "a refusal wrote to stdout: $out"
	grep -q "$missing" "$work/err" || fail "the refusal does not name $missing: $(cat "$work/err")"
done

set +e
out=$(GITHUB_APP_ID=x GITHUB_APP_PRIVATE_KEY="not a key" "$helper" 2>/dev/null)
code=$?
set -e
(( code != 0 )) || fail "a malformed key was not refused"
[[ -z "$out" ]] || fail "a malformed key wrote to stdout: $out"

echo "github-app-jwt: ok"
