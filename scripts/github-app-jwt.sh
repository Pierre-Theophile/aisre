#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# A credential helper for `feed github --app-assertion-command`: prints one fresh GitHub App JWT
# on stdout and nothing else.
#
# The connector has no flag for the App's private key, on purpose (docs/connectors/github.md §7.2).
# A run lasting longer than ten minutes needs a new assertion at every token renewal, so it runs a
# helper, and the helper decides where the key lives. This one is the simplest such helper, for an
# operator whose secret store hands the key over as an environment variable:
#
#   GITHUB_APP_ID           the App's client ID (Iv23…) or numeric App ID; it becomes the JWT's `iss`
#   GITHUB_APP_PRIVATE_KEY  the App's PEM private key, the whole file's contents
#
# The key never touches disk. openssl reads it through a pipe, and only for the one signature.
# Where a KMS or a vault can sign instead, use a helper that asks it to: then the key never enters
# this process at all, which is the better of the two shapes §7.2 describes.
#
# GitHub caps an App JWT at ten minutes and rejects one whose `iat` is in its future, so `iat` is
# backdated sixty seconds for clock skew and `exp` is nine minutes out.
set -euo pipefail

die() { printf 'github-app-jwt: %s\n' "$*" >&2; exit 2; }

[[ -n "${GITHUB_APP_ID:-}" ]] || die "GITHUB_APP_ID is not set"
[[ -n "${GITHUB_APP_PRIVATE_KEY:-}" ]] || die "GITHUB_APP_PRIVATE_KEY is not set"
command -v openssl >/dev/null || die "openssl is not installed"

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

now=$(date +%s)
header=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)
payload=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((now - 60))" "$((now + 540))" "$GITHUB_APP_ID" | b64url)
signature=$(printf '%s.%s' "$header" "$payload" |
	openssl dgst -sha256 -sign <(printf '%s\n' "$GITHUB_APP_PRIVATE_KEY") -binary | b64url) ||
	die "signing failed: is GITHUB_APP_PRIVATE_KEY a PEM private key?"
[[ -n "$signature" ]] || die "signing produced nothing: is GITHUB_APP_PRIVATE_KEY a PEM private key?"

printf '%s.%s.%s\n' "$header" "$payload" "$signature"
