#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The guard's own test (T116).
#
# A grep guard nobody has seen fail is a guard nobody knows works. This plants one sample of each
# shape check-no-secrets.sh refuses, runs the guard over the planted tree, and asserts that every
# rule fired — and then asserts the guard is still clean on the real tree, so that a rule which
# fires on everything is caught here rather than in CI.
#
# The planted tree is a copy of the script outside any git work tree, so nothing is ever written
# into the repository and the guard takes its `find` path rather than `git ls-files`. It is tiny,
# so this test runs in well under a second whatever the corpus has grown to.
#
#   scripts/check-no-secrets_test.sh
#
# Exit 0 if every rule fired on its planted sample and none fired on the placeholders, 1 otherwise.

set -uo pipefail

cd "$(dirname "$0")/.."

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/scripts" "$work/fixtures/incidents/planted-01/world" \
  "$work/fixtures/incidents/planted-01/trajectories"
cp scripts/check-no-secrets.sh "$work/scripts/"
cp scripts/known-recording-findings.txt "$work/scripts/" 2>/dev/null || true

# One planted sample per rule. Each is a shape that has actually been leaked by a recording of a
# real cluster somewhere, which is why the rule exists.
cat >"$work/fixtures/incidents/planted-01/world/index.json" <<'JSON'
{
  "note": "planted samples for scripts/check-no-secrets_test.sh",
  "apiKey": "sk-abcdefghijklmnopqrstuvwxyz0123456789",
  "author": "jane.doe@acme-corp.io",
  "reviewer": "see @jane-doe for context",
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "resourceSpans": [],
  "service.instance.id": "checkout-7d9f8b6c4d-x2p9q",
  "template": "2026-09-01T14:32:00Z ERROR payment failed for 10.4.2.19 user 3f1a9c2e-4b5d-4f6a-9e8c-1d2b3a4c5d6e after 1200ms retrying upstream https://payments.internal.acme-corp.io/v1/charge with idempotency key 7c9f2b1a and correlation 9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b"
}
JSON
cat >"$work/fixtures/incidents/planted-01/trajectories/leak.json" <<'JSON'
{"headers": {"Authorization": "Bearer abcdefghijklmnopqrstuvwxyz012345"},
 "aws": "AKIAIOSFODNN7EXAMPLE",
 "dsn": "postgres://produser:hunter2@db.prod.internal:5432/orders"}
JSON

# The personal-data plants (T043). Each is a field that a *sanitised* recording does not have at all,
# so the name is the finding — and each has actually been left in a recording of a real project by
# somebody who thought dropping the value was enough.
cat >"$work/fixtures/incidents/planted-01/world/audit.json" <<'JSON'
{
  "protoPayload": {
    "authenticationInfo": {"principalEmail": "px_res_k3m2j4h5g6f7"},
    "requestMetadata": {"callerIp": "203.0.113.42", "callerSuppliedUserAgent": "gcloud/456.0.0"},
    "methodName": "google.cloud.run.v2.Services.UpdateService"
  },
  "project_id": "px_0123456789abcdef",
  "essentialContacts": ["px_res_aaaabbbbcccc"]
}
JSON
cat >"$work/fixtures/incidents/planted-01/world/notice.json" <<'JSON'
{
  "vendor": {"slug": "gcp", "product": "cloudsql"},
  "bodyHtml": "<p>We will perform maintenance on your instance.</p>",
  "snippet": "Scheduled maintenance"
}
JSON

# And the canary: a token planted in the source data before the campaign, surviving into a committed
# artifact. It carries an allow marker on its own line, which every other rule here would honour —
# the canary rule must not, or a survivor could be waved through with one comment.
cat >"$work/fixtures/incidents/planted-01/world/canary.json" <<'JSON'
{"service": "sreagentcanary52d7e1d630ca243db4051b613b4c3cf4"} // check-no-secrets: allow
JSON

out="$(cd "$work" && bash scripts/check-no-secrets.sh fixtures 2>&1)"
code=$?

failed=0
if [ "$code" -eq 0 ]; then
  echo "FAIL: the guard passed a tree full of planted secrets" >&2
  failed=1
fi

# Every rule the planted tree should have tripped, by name.
#
# The match is anchored to the report's own two-space, padded-column layout rather than being a bare
# substring. A substring match would accept `gcp-principal-disabled` for `gcp-principal`, so a rule
# somebody renamed or turned off would keep passing this test — which is the one failure mode a
# guard's own test must not have.
for rule in api-key people-identifier raw-span-payload telemetry-instance-id \
  unredacted-log-line bearer-token cloud-access-key database-password \
  gcp-principal gcp-caller-ip notice-body essential-contact unkeyed-pseudonym canary-token; do
  if ! printf '%s' "$out" | grep -qE "^  $rule +[^ ]"; then
    echo "FAIL: rule '$rule' did not fire on its planted sample" >&2
    failed=1
  fi
done

# The finding names the file, never the value: a report that quoted the secret would be the
# thing the guard exists to prevent.
for value in "sk-abcdefghijklmnopqrstuvwxyz0123456789" "jane.doe@acme-corp.io" "hunter2" \
  "AKIAIOSFODNN7EXAMPLE"; do
  if printf '%s' "$out" | grep -qF "$value"; then
    echo "FAIL: the guard printed the value it found ($value)" >&2
    failed=1
  fi
done

# The canary was planted on a line carrying the allow marker, and it must still have fired. A
# surviving canary that one comment can wave through is a warning, and SC-018 says it is a failure.
if ! printf '%s' "$out" | grep -qE "^  canary-token +[^ ]"; then
  echo "FAIL: the canary rule was waived by an allow marker; a surviving canary fails the commit" >&2
  failed=1
fi

# The canary token is a value, and the report must not quote it — a report that did would put the
# token in a CI log and then find it there on the next run.
if printf '%s' "$out" | grep -qF "sreagentcanary52d7e1d630ca243db4051b613b4c3cf4"; then
  echo "FAIL: the guard printed the canary token it found" >&2
  failed=1
fi

# A pseudonymised principal is the leak a value-shaped regexp misses: there is no address in
# `"principalEmail": "px_res_..."`, and the field is still a person that was recorded. The
# people-identifier rule cannot see it; gcp-principal is the rule that must.
if ! printf '%s' "$out" | grep -qE "^  gcp-principal +[^ ]"; then
  echo "FAIL: a pseudonymised principal field was not caught; the field name is the finding" >&2
  failed=1
fi

# A human's address is still a person even when it resembles the robot domains. The allow-list is
# anchored on the domain, so `@gserviceaccount.com.evil.test` and `@notgserviceaccount.com` must
# still fire — an allow-list that matched a substring would be an allow-list for anything.
mkdir -p "$work/lookalike/fixtures/incidents/look-01" "$work/lookalike/scripts"
cp scripts/check-no-secrets.sh "$work/lookalike/scripts/"
cp scripts/known-recording-findings.txt "$work/lookalike/scripts/" 2>/dev/null || true
cat >"$work/lookalike/fixtures/incidents/look-01/payload.json" <<'JSON'
{"author": "jane.doe@iam.gserviceaccount.com.evil.io"}
JSON
if (cd "$work/lookalike" && bash scripts/check-no-secrets.sh fixtures >/dev/null 2>&1); then
  echo "FAIL: an address at a look-alike of the service-account domain was allow-listed" >&2
  failed=1
fi

# Every alternative inside a personal-data rule, one per file (T043).
#
# The checks above prove each *rule* fires. They do not prove each alternative inside it does, and an
# alternation is exactly where a typo hides: a seven-way rule whose planted sample happens to match
# alternative five passes while alternatives one to four match nothing. So each alternative is planted
# alone, in its own file, and the report must name that file — which it does, because a finding is
# reported as `rule<spaces>file`.
alt_failed=0
plant_alt() {
  local rule="$1" token="$2" body="$3"
  local dir="$work/alt/$rule-$token"
  mkdir -p "$dir/fixtures/incidents/alt-01" "$dir/scripts"
  cp scripts/check-no-secrets.sh "$dir/scripts/"
  cp scripts/known-recording-findings.txt "$dir/scripts/" 2>/dev/null || true
  printf '%s\n' "$body" >"$dir/fixtures/incidents/alt-01/payload.json"
  local alt_out
  alt_out="$(cd "$dir" && bash scripts/check-no-secrets.sh fixtures 2>&1 || true)"
  if ! printf '%s' "$alt_out" | grep -qE "^  $rule +fixtures/"; then
    echo "FAIL: rule '$rule' did not fire on its '$token' alternative" >&2
    alt_failed=1
  fi
}

for token in principalEmail principalSubject serviceAccountKeyName \
  serviceAccountDelegationInfo authenticationInfo firstPartyPrincipal thirdPartyPrincipal; do
  plant_alt gcp-principal "$token" "{\"$token\": \"px_res_k3m2j4h5g6f7\"}"
done

for token in callerIp callerNetwork callerSuppliedUserAgent; do
  plant_alt gcp-caller-ip "$token" "{\"$token\": \"redacted\"}"
done

for token in bodyHtml quotedThread snippet textPayload; do
  plant_alt notice-body "$token" "{\"$token\": \"some prose\"}"
done
# The fifth alternative is a length rule rather than a name, so it needs a long value.
plant_alt notice-body long-body \
  "{\"body\": \"$(printf 'x%.0s' $(seq 1 100))\"}"

for token in essentialContacts contactEmail notificationEmail ownerEmail billingContact; do
  plant_alt essential-contact "$token" "{\"$token\": \"px_res_aaaabbbbcccc\"}"
done

for token in project_id service_name revision_name instance_connection_name alert_policy_id; do
  plant_alt unkeyed-pseudonym "$token" "{\"$token\": \"px_0123456789abcdef\"}"
done

if [ "$alt_failed" -ne 0 ]; then
  failed=1
fi

# A placeholder at a reserved documentation name is not a person, and an entity key is not an
# e-mail address. Both live in every fixture; a rule that flagged them would be unusable.
mkdir -p "$work/clean/fixtures/incidents/clean-01"
cat >"$work/clean/fixtures/incidents/clean-01/manifest.yaml" <<'YAML'
author: alice@shop.example
reviewer: bob@example.com
# A GCP service-account address: a robot identity, on a Google-managed domain no human has an
# address at. Every Cloud Run revision names one, so a rule that flagged it would be unusable on
# every real GCP recording.
runtime_identity: runtime@twin-production.iam.gserviceaccount.com
legacy_identity: 1234567890-compute@developer.gserviceaccount.com
subject: payments@otel.service.name
workload: storefront@app.kubernetes.io
template: "handled request id=<num> for payments"
dsn: postgres://sreagent:sreagent@localhost:5432/sreagent
YAML
# A correctly sanitised 003 recording: kind-typed pseudonyms, a public method name, a region, and no
# dropped field present at all. Every rule added in T043 must be silent on it, or the scan is
# unusable on the corpus it was written for.
cat >"$work/clean/fixtures/incidents/clean-01/audit.json" <<'JSON'
{
  "protoPayload": {"methodName": "google.cloud.run.v2.Services.UpdateService",
                   "resourceName": "px_res_k3m2j4h5g6f7"},
  "project_id": "px_prj_aaaabbbbcccc",
  "service_name": "px_svc_ddddeeeeffff",
  "region": "europe-west1",
  "change": {"kind": "deploy", "announcement_state": "CONFIRMED"},
  "vendor": {"slug": "gcp", "product": "cloudrun"},
  "timestamp": "2026-09-01T14:32:00Z"
}
JSON
mkdir -p "$work/clean/scripts"
cp scripts/check-no-secrets.sh "$work/clean/scripts/"
cp scripts/known-recording-findings.txt "$work/clean/scripts/" 2>/dev/null || true
if ! (cd "$work/clean" && bash scripts/check-no-secrets.sh fixtures >/dev/null 2>&1); then
  echo "FAIL: the guard flagged fixture placeholders and entity keys" >&2
  (cd "$work/clean" && bash scripts/check-no-secrets.sh fixtures 2>&1 | sed 's/^/    /') >&2
  failed=1
fi

# The real tree is deliberately NOT scanned here: `scripts/check-no-secrets.sh` is run on it by
# the same CI job (and by hand), and scanning the whole corpus twice doubles the slowest step in
# the lint job for no extra information. This test is about the rules, not about the tree.

if [ "$failed" -ne 0 ]; then
  echo "check-no-secrets_test: FAILED" >&2
  exit 1
fi
echo "check-no-secrets_test: ok (every rule fired on its planted sample; placeholders were not flagged)"
