#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Is the organisation's side of the 003 and 004 recording campaigns in place?
#
#   scripts/campaign-preflight.sh [003|004|all]      (default: all)
#
# Everything that blocks 003 T156 and 004 T114 is a credential, a scope or a person, and none of
# it can come from this repository. This script answers, in one run, which of those are present
# and whether each connector's own read-only gate accepts the credential it was given. It runs
# only the `--dry-run` gates: no operation from any published surface, no event, no recording, no
# graph connection. Starting the campaign stays a human decision (quickstart §7 and §8 of each
# feature), because the signatory and the scope are recorded in the campaign's own account of
# itself and this script must not guess either.
#
# Inputs are environment variables, because that is how a cloud session's environment hands over
# secrets. No value is ever printed; only whether it is set. The full list, and who grants each,
# is docs/campaigns/003-004-access-request.md.
#
#   both      SRE_AGENT_ORG, SRE_AGENT_CORPUS_KEY, SRE_AGENT_SIGNATORY
#   003       GOOGLE_APPLICATION_CREDENTIALS (a path) or GCP_SERVICE_ACCOUNT_JSON (the key's
#             contents, written to a private temporary file for the gate and removed on exit),
#             GCP_PROJECTS, GCP_REGIONS, and at least one mailbox in $VENDORS_CONFIG
#             (default config/vendors.yaml) whose password_env / token_env is set
#   004       GITHUB_APP_ID, GITHUB_APP_PRIVATE_KEY, GITHUB_INSTALLATION_ID, VERCEL_TOKEN;
#             VERCEL_TEAM where the token is a personal one
#
# Exit 0 when every input for the requested features is present and every gate accepted;
# 1 otherwise, after reporting everything rather than stopping at the first gap.
set -uo pipefail

which=${1:-all}
case "$which" in 003 | 004 | all) ;; *) echo "usage: $0 [003|004|all]" >&2; exit 2 ;; esac

root=$(cd "$(dirname "$0")/.." && pwd)
bin="$root/bin/aisre"
vendors=${VENDORS_CONFIG:-$root/config/vendors.yaml}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
missing=0

ok() { printf '  ok       %s\n' "$*"; }
miss() { printf '  MISSING  %s\n' "$*"; missing=1; }
need() { # need VAR "what it is" "what it unblocks"
	if [[ -n "${!1:-}" ]]; then ok "$1"; else miss "$1 — $2 (unblocks $3)"; fi
}
gate() { # gate "label" cmd...
	local label=$1 out code
	shift
	out=$("$@" 2>&1)
	code=$?
	if (( code == 0 )) && ! grep -q 'REFUSED' <<<"$out"; then
		ok "$label: the read-only gate accepted"
	else
		miss "$label: the read-only gate refused"
		sed 's/^/             /' <<<"$out" | grep -v '^ *$' | head -8
	fi
}

if [[ ! -x "$bin" ]]; then
	echo "building bin/aisre…"
	make -C "$root" build >/dev/null || { echo "make build failed" >&2; exit 1; }
fi

echo "both campaigns"
need SRE_AGENT_ORG "the organisation slug every source id carries" "T156, T114"
need SRE_AGENT_CORPUS_KEY "the corpus HMAC key the connectors sanitise with before anything touches disk (FR-137)" "T156, T114"
need SRE_AGENT_SIGNATORY "a named human who signs the manifest (003 FR-140, 004 FR-064); never automation" "T157, T115"

if [[ "$which" == 003 || "$which" == all ]]; then
	echo
	echo "003 — GCP and vendor notices (T156, T157, then T172, T173)"
	creds=${GOOGLE_APPLICATION_CREDENTIALS:-}
	if [[ -z "$creds" && -n "${GCP_SERVICE_ACCOUNT_JSON:-}" ]]; then
		creds="$work/gcp.json"
		(umask 077 && printf '%s' "$GCP_SERVICE_ACCOUNT_JSON" >"$creds")
	fi
	if [[ -n "$creds" ]]; then ok "GCP credential"; else
		miss "GOOGLE_APPLICATION_CREDENTIALS or GCP_SERVICE_ACCOUNT_JSON — the read-only service account of docs/connectors/gcp.md §1 (unblocks T156)"
	fi
	need GCP_PROJECTS "comma-separated projects in scope; no code assumes one (FR-131)" "T156"
	need GCP_REGIONS "comma-separated regions in scope" "T156"
	if [[ -n "$creds" && -n "${GCP_PROJECTS:-}" && -n "${GCP_REGIONS:-}" && -n "${SRE_AGENT_ORG:-}" ]]; then
		gate "feed gcp" env GOOGLE_APPLICATION_CREDENTIALS="$creds" "$bin" feed gcp \
			--org "$SRE_AGENT_ORG" --projects "$GCP_PROJECTS" --regions "$GCP_REGIONS" --dry-run
	fi

	# The mailbox's credential is named in the allowlist and read from the environment; the
	# allowlist ships with none, because the address is the organisation's to choose.
	envs=$(grep -E '^[[:space:]]*-?[[:space:]]*(password_env|token_env):' "$vendors" |
		sed -E 's/.*_env:[[:space:]]*"?([A-Za-z0-9_]+)"?.*/\1/')
	if grep -qE '^mailboxes:[[:space:]]*\[\][[:space:]]*$' "$vendors" || [[ -z "$envs" ]]; then
		miss "a notice mailbox in $(basename "$vendors") with its credential variable — authorised by a named owner (FR-132); see docs/connectors/vendor-notice.md §3 (unblocks T156)"
	else
		for v in $envs; do need "$v" "the notice mailbox credential $(basename "$vendors") names" "T156"; done
	fi
	if [[ -n "${SRE_AGENT_ORG:-}" ]]; then
		gate "feed vendor-notice" "$bin" feed vendor-notice --allowlist "$vendors" --org "$SRE_AGENT_ORG" --dry-run
	fi
fi

if [[ "$which" == 004 || "$which" == all ]]; then
	echo
	echo "004 — GitHub and Vercel deployments (T114–T116, then T112, T125–T127; T034 with a project-scoped token)"
	need GITHUB_APP_ID "the read-only App's client ID; permissions per docs/connectors/github.md §1" "T114"
	need GITHUB_APP_PRIVATE_KEY "the App's PEM key, read only by scripts/github-app-jwt.sh" "T114"
	need GITHUB_INSTALLATION_ID "the App's installation on the organisation, with its repository selection" "T114"
	if [[ -n "${GITHUB_APP_ID:-}" && -n "${GITHUB_APP_PRIVATE_KEY:-}" && -n "${GITHUB_INSTALLATION_ID:-}" ]]; then
		gate "feed github" "$bin" feed github --installation "$GITHUB_INSTALLATION_ID" \
			--app-assertion-command "$root/scripts/github-app-jwt.sh" --dry-run
	fi
	need VERCEL_TOKEN "a Vercel access token; project-scoped where possible (docs/connectors/vercel.md §1)" "T114, T034"
	if [[ -n "${VERCEL_TOKEN:-}" ]]; then
		# Vercel reports nothing about a token's write capability, and this dry-run issues no call, so
		# "accepted" here says the command runs, not that the token works; read-only is the
		# operator's --assert-read-only on the live run (docs/connectors/vercel.md §1).
		gate "feed vercel (issues no call: the token itself is first exercised by the live run)" "$bin" feed vercel --vercel-token "$VERCEL_TOKEN" ${VERCEL_TEAM:+--team "$VERCEL_TEAM"} --dry-run
	fi
fi

echo
if (( missing )); then
	echo "not ready: the items marked MISSING are the organisation's to provide"
	exit 1
fi
echo "ready: run quickstart §7 then §8 of specs/003-gcp-integration and specs/004-deploy-feeders"
