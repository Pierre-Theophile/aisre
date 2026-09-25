#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Refuse a secret in a recording (T090, constitution VII and VIII, FR-046).
#
# Fixtures are recordings of real clusters. A Kubernetes Secret's `data` is base64, not
# encryption; `kubectl.kubernetes.io/last-applied-configuration` carries the whole object an
# operator applied, secrets included; and a private key or a cloud access key pasted into a
# deployment manifest is a credential in git for ever. The graph already refuses a secret's
# material at ingestion (ReasonSecretValue), but a payload that never reaches the graph is not
# covered by that, and a recording is committed before anyone replays it.
#
# This is a grep, deliberately. It is not a secret scanner and does not pretend to be one: it
# catches the shapes this project actually produces, fails the build when it sees a new one, and
# says what to do about it. Anything cleverer would be trusted, and a scanner that is trusted and
# wrong is worse than a grep that is dumb and honest.
#
# Two severities:
#
#   * Hard rules — a private key, a cloud access key, a bearer token, a plaintext `stringData`,
#     a database password that is not the published local one — always fail. There is no
#     legitimate reason for one of these to be in the tree.
#
#     Since T116 the hard rules also cover what a *recording* leaks rather than what a manifest
#     does: a vendor API key, a person (an e-mail address, an @handle) and a raw telemetry body
#     (an unredacted log line, a raw span payload, a `service.instance.id`). A corpus incident is
#     shared with graders and replayed by strangers; the redaction contract (FR-038,
#     internal/investigation/backend/redact.go) says what may be in one, and these rules are that
#     contract read backwards.
#
#     Since T043 they also cover the personal-data half of FR-138: a GCP audit principal field, a
#     `callerIp`, a vendor notice body, an Essential Contacts address, an untyped `px_` token, and a
#     seeded canary. Those rules match *field names* rather than values, because a sanitised
#     recording has no dropped field at all — so the name is the finding, and a name survives the
#     leak a value-shaped regexp misses (`"principalEmail": "px_res_abc"` is a pseudonymised person
#     and looks like nothing). The canary rule is unwaivable: no marker, no baseline.
#
#     This is the scan FR-138 requires be "implemented separately from the sanitiser so that one
#     defect cannot both leak and pass". Separately means it is a grep, in another language, that
#     shares no code with internal/sanitise and must never import it. internal/sanitise has its own
#     in-process check (AssertArtifact); that one is not this one, and neither substitutes.
#
#   * Baselined rules — a Secret's base64 `data` value, and the applied-configuration
#     annotation — fail unless the exact occurrence is listed in the baseline beside this
#     script. The baseline records what a human has looked at and accepted (a kind-demo
#     placeholder, say), by file and by digest, never by value. Anything new fails.
#
#   scripts/check-no-secrets.sh                      # the default roots
#   scripts/check-no-secrets.sh path [path...]       # explicit roots
#   UPDATE_BASELINE=1 scripts/check-no-secrets.sh    # rewrite the baseline (review the diff!)
#
# Exit 0 clean, 1 with findings.

set -euo pipefail

cd "$(dirname "$0")/.."

BASELINE_FILE="scripts/known-recording-findings.txt"

# The roots that hold recorded or deployed material. Source code is not scanned: a credential in
# a .go file is a different problem with a different fix, and `go test` is where it shows up.
DEFAULT_ROOTS=(fixtures internal/feeders deploy)

# Files allowed to contain a matching shape, because the match is the point.
#
# Go sources are excluded, `_test.go` above all: the regression test for a leak has to contain
# the shape it is testing for (internal/feeders/k8s/informers_test.go is exactly that), and a
# credential in Go source is a different problem with a different fix — code review, and the
# graph's own ReasonSecretValue path. These roots are about *recorded and deployed material*.
ALLOWLIST_RE='^(scripts/|docs/security\.md$|.*README\.md$)|\.go$'

# A file may declare one line exempt by carrying this marker on it or on the line above. It is
# for a demo manifest that has to show the shape of a Secret; the marker makes the exemption a
# reviewable line in the file rather than a rule in this script.
ALLOW_MARKER='check-no-secrets: allow'

# People identifiers that are not people (T116).
#
# The graph's own entity keys are spelled `<name>@<attribute.key>` — `payments@otel.service.name`,
# `redis@app.kubernetes.io` — and an e-mail regexp cannot tell one from a person's address. The
# allow-list is therefore closed and spelled out here: the published attribute namespaces, the
# reserved documentation names a fixture author may use for a placeholder (RFC 2606 and RFC 6761:
# `.example`, `.invalid`, `.test`, `.localhost`, and example.com/org/net — `alice@shop.example` is
# a placeholder, `alice@shop.io` is a person), and the three Slack group mentions that name
# nobody. Anything else is a person in a corpus that is shared with graders.
# A GCP service-account address is a robot identity, not a person: the domain is Google-managed and
# no human has an address at it. Every Cloud Run revision names the identity it runs as, so this is
# not a concession to the twins — it is a rule every real GCP recording needs. Principal *fields* are
# caught by name regardless of value (the gcp-principal rule), so allow-listing the domain does not
# weaken the detection that matters.
PEOPLE_ALLOWED_RE='@(otel|k8s|app|service|host|container|cloud|db|server|http|url|process|net|node|pod|namespace|telemetry|deployment|statefulset|daemonset|faas|messaging|rpc|aws|gcp|azure)\.|@([A-Za-z0-9.-]+\.)?(example|invalid|test|localhost)($|[^A-Za-z0-9.-])|@example\.(com|org|net)($|[^A-Za-z0-9.-])|@(here|channel|everyone)($|[^A-Za-z0-9-])|@[A-Za-z0-9.-]*\.?(iam\.gserviceaccount\.com|developer\.gserviceaccount\.com|appspot\.gserviceaccount\.com)($|[^A-Za-z0-9.-])'

# The shapes a person is written in: an e-mail address, or an @handle at the start of a word.
PEOPLE_RE='[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}|(^|[[:space:]])@[A-Za-z][A-Za-z0-9._-]{2,}'

# A masked template is short by construction (the longest in the corpus is ~70 characters). A
# "template" far longer than that is a raw log line that never went through the miner.
TEMPLATE_MAX=200

roots=("$@")
if [ ${#roots[@]} -eq 0 ]; then
  roots=()
  for root in "${DEFAULT_ROOTS[@]}"; do
    [ -e "$root" ] && roots+=("$root")
  done
fi
if [ ${#roots[@]} -eq 0 ]; then
  echo "check-no-secrets: nothing to scan" >&2
  exit 0
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
: >"$work/hard"
: >"$work/baselined"

digest() { printf '%s' "$1" | shasum -a 256 2>/dev/null | cut -c1-16; }

# files lists every text file under the roots, skipping the allowlist. Binary payloads (recorded
# OTLP is protobuf) are skipped: base64 noise inside them is not a finding.
files() {
  if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git ls-files -z --cached --others --exclude-standard -- "${roots[@]}"
  else
    find "${roots[@]}" -type f -print0
  fi | while IFS= read -r -d '' file; do
    [[ "$file" =~ $ALLOWLIST_RE ]] && continue
    if [ -f "$file" ] && LC_ALL=C grep -qI . "$file" 2>/dev/null; then
      printf '%s\0' "$file"
    fi
  done
}

files >"$work/files"

# scan_unwaivable <rule> <why> <extended-regexp>
#
# Like scan, except the per-line allow marker does not apply (T043, FR-139).
#
# Every other rule here can be waived on a line, because every other rule matches a shape that a
# demo manifest legitimately has to show. A canary matches a token that was planted in the source
# data before the campaign, in the place the real value lives, precisely so that its survival cannot
# be explained away — and "this canary survived but it is fine" is not a judgement anybody can make.
# So there is no marker, no baseline entry and no UPDATE_BASELINE for it: a surviving canary fails
# the commit (SC-018).
scan_unwaivable() {
  local rule="$1" why="$2" pattern="$3"
  local hits
  hits="$(xargs -0 grep -lE "$pattern" <"$work/files" 2>/dev/null || true)"
  [ -z "$hits" ] && return 0
  while IFS= read -r file; do
    [ -z "$file" ] && continue
    local matched
    matched="$(grep -ohE "$pattern" "$file" | sort -u | tr '\n' ' ')"
    [ -z "${matched// /}" ] && continue
    printf '%s\t%s\t%s\t%s\n' "$rule" "$file" "$(digest "$matched")" "$why" >>"$work/hard"
  done <<<"$hits"
}

# scan <severity> <rule> <why> <extended-regexp>
#
# Matches are recorded as `rule<TAB>file<TAB>digest`, and only ever as a digest: a baseline that
# quoted the values it accepts would be the file this script exists to prevent.
scan() {
  local severity="$1" rule="$2" why="$3" pattern="$4"
  local hits
  hits="$(xargs -0 grep -lE "$pattern" <"$work/files" 2>/dev/null || true)"
  [ -z "$hits" ] && return 0
  while IFS= read -r file; do
    [ -z "$file" ] && continue
    # Drop matches on a line the file itself exempts, and on the line after a marker.
    local matched
    matched="$(grep -nE "$pattern" "$file" |
      while IFS=: read -r lineno _; do
        if sed -n "${lineno}p;$((lineno > 1 ? lineno - 1 : 1))p" "$file" |
          grep -qF "$ALLOW_MARKER"; then
          continue
        fi
        sed -n "${lineno}p" "$file" | grep -ohE "$pattern"
      done | sort -u | tr '\n' ' ')"
    [ -z "${matched// /}" ] && continue
    printf '%s\t%s\t%s\t%s\n' "$rule" "$file" "$(digest "$matched")" "$why" \
      >>"$work/$severity"
  done <<<"$hits"
}

echo "check-no-secrets: scanning ${roots[*]}"

# --- hard rules -------------------------------------------------------------------------

scan hard "private-key" \
  "a private key header. Rotate the key, then remove it; a fixture never needs one." \
  '-----BEGIN [A-Z ]*PRIVATE KEY-----'

scan hard "cloud-access-key" \
  "a long-lived cloud access key. Rotate it, then remove it." \
  '(AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}|AIza[0-9A-Za-z_-]{35}|gh[pousr]_[A-Za-z0-9]{36}|xox[baprs]-[A-Za-z0-9-]{10,}'

scan hard "bearer-token" \
  "a bearer token or JWT. Replace it with a placeholder; mint a fresh one with 'aisre dev-token --dev'." \
  'eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}|[Aa]uthorization["'"'"']?[[:space:]]*[:=][[:space:]]*["'"'"']?[Bb]earer[[:space:]]+[A-Za-z0-9._-]{12,}'

scan hard "k8s-stringdata" \
  "a Kubernetes Secret stringData value: that is plaintext. Keep the key names, replace every value." \
  '"stringData"[[:space:]]*:[[:space:]]*\{[[:space:]]*"[^"]+"[[:space:]]*:[[:space:]]*"[^"]+"|^[[:space:]]*stringData:[[:space:]]*$'

scan hard "database-password" \
  "a database password. Use the published local credential (sreagent:sreagent@localhost) or an environment variable." \
  'postgres(ql)?://[A-Za-z0-9_.-]+:[A-Za-z0-9_.%-]+@[A-Za-z0-9_.:-]+'

scan hard "api-key" \
  "a vendor API key. Rotate it, then remove it; no recording and no fixture needs one." \
  '\b(sk|pk|rk)-[A-Za-z0-9_-]{20,}|\bglpat-[A-Za-z0-9_-]{16,}|\bnpm_[A-Za-z0-9]{30,}|\bhf_[A-Za-z0-9]{30,}|\b(api[_-]?key|apikey|access[_-]?token|secret[_-]?key)["'"'"']?[[:space:]]*[:=][[:space:]]*["'"'"']?[A-Za-z0-9/_+-]{20,}'

# --- people, and telemetry bodies (T116) ------------------------------------------------
#
# These are the two classes the redaction contract drops outright (FR-038): people identifiers
# are dropped, never pseudonymised, and log bodies are reduced to masked templates. A recording
# that carries either is a recording that cannot be shared, and it is committed to git for ever.

scan hard "people-identifier" \
  "a person: an e-mail address or an @handle. The redaction contract drops people identifiers outright (FR-038). Replace it with a placeholder at example.com." \
  "$PEOPLE_RE"

scan hard "raw-span-payload" \
  "a raw span or trace payload. Recordings carry digests and pseudonymised join keys (px_…), never raw spans." \
  '"(traceId|trace_id|spanId|span_id|parentSpanId|parent_span_id)"[[:space:]]*:[[:space:]]*"(0x)?[0-9a-fA-F]{16,}"|"(resourceSpans|scopeSpans|startTimeUnixNano|endTimeUnixNano|droppedAttributesCount)"'

scan hard "telemetry-instance-id" \
  "a service.instance.id value. It names one production process; the contract pseudonymises it (px_…)." \
  'service\.instance\.id["'"'"']?[[:space:]]*[:=][[:space:]]*["'"'"']?[^"'"'"',}[:space:]]+'

scan hard "unredacted-log-line" \
  "a log template longer than the redaction contract allows, or one still carrying an IP, a UUID or an e-mail. Log bodies are reduced to masked templates before recording (FR-038)." \
  '"template"[[:space:]]*:[[:space:]]*"[^"]{'"$TEMPLATE_MAX"',}"|"template"[[:space:]]*:[[:space:]]*"[^"]*([0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,})'

# --- the personal-data scan (T043, FR-138, SC-019) ---------------------------------------
#
# FR-138 wants two scans at commit, "implemented separately from the sanitiser so that one defect
# cannot both leak and pass". This is the personal-data half, and separately means what it says: it is
# a grep, in another language, that knows nothing about internal/sanitise and shares no code with it.
# If the disposition table grows a hole, these rules still fire; if these rules have a hole, the table
# still drops the field. Neither is the other's substitute, and neither is allowed to import the other.
#
# The rules match *field names* rather than values, which is the opposite of the people-identifier
# rule above and deliberately so. A sanitised recording does not contain a dropped field at all, so
# the presence of the name is the finding — and a name survives a leak that a value-shaped regexp
# misses, because `"principalEmail": "px_res_abc"` is a pseudonymised person and looks like nothing.

scan hard "gcp-principal" \
  "a GCP audit principal field. Principals are dropped, never hashed or renamed (FR-135): a sanitised audit entry has no such field at all, so the name itself is the finding." \
  '"?(principalEmail|principalSubject|serviceAccountKeyName|serviceAccountDelegationInfo|authenticationInfo|firstPartyPrincipal|thirdPartyPrincipal)"?[[:space:]]*:'

scan hard "gcp-caller-ip" \
  "a callerIp or callerNetwork field. That is where a person was sitting when they deployed, not a machine that serves traffic; it is dropped, not pseudonymised." \
  '"?(callerIp|callerNetwork|callerSuppliedUserAgent)"?[[:space:]]*:'

scan hard "notice-body" \
  "a vendor notice body. A body is never written to the graph, disk, a log or any artifact, including on a failed run (FR-071); the recording carries typed fields and a pointer." \
  '"?(bodyHtml|quotedThread|snippet|textPayload)"?[[:space:]]*:|"body"[[:space:]]*:[[:space:]]*"[^"]{80,}"'

scan hard "essential-contact" \
  "an Essential Contacts or contact-email field. Google addresses platform notices to named principals; the addresses are people and are dropped (FR-072, FR-132a)." \
  '"?(essentialContacts|contactEmail|notificationEmail|ownerEmail|billingContact)"?[[:space:]]*:'

scan hard "unkeyed-pseudonym" \
  "a pseudonym with no kind tag. This feature's tokens are px_<kind>_<base32>; an untyped px_ token in a 003 recording means a service and an instance sharing a name would have merged (contract §2.3)." \
  '"(project_id|service_name|revision_name|instance_connection_name|alert_policy_id)"[[:space:]]*:[[:space:]]*"px_[0-9a-f]{16}"'

scan_unwaivable "canary-token" \
  "a canary token seeded into the source data before the campaign. A surviving canary fails the commit rather than raising a warning (FR-139, SC-018): every rule above it was wrong about this value, so the recording is not safe to keep. There is no baseline entry and no allow marker for this rule." \
  'sreagentcanary[0-9a-f]{8,}'

# --- baselined rules --------------------------------------------------------------------

scan baselined "k8s-secret-data" \
  "a Kubernetes Secret data value. Keep the key names and the resourceVersion; replace the value." \
  '"data"[[:space:]]*:[[:space:]]*\{[[:space:]]*"[^"]+"[[:space:]]*:[[:space:]]*"[A-Za-z0-9+/=]{16,}"'

scan baselined "last-applied-configuration" \
  "kubectl's last-applied-configuration annotation carries the whole applied object, Secret values included." \
  'last-applied-configuration'

# Two rules match shapes that are legitimately everywhere in this repository — the published
# local DSN, and the graph's `<name>@<attribute.key>` entity keys — so their findings are
# filtered back out here rather than excluded by a regexp nobody can read.
if [ -s "$work/hard" ]; then
  : >"$work/hard.kept"
  while IFS=$'\t' read -r rule file dgst why; do
    # A file whose only DSN is the published local one is not a finding; it is in every document
    # here on purpose.
    if [ "$rule" = "database-password" ] &&
      ! grep -ohE 'postgres(ql)?://[A-Za-z0-9_.-]+:[A-Za-z0-9_.%-]+@' "$file" |
      grep -qv 'sreagent:sreagent@'; then
      continue
    fi
    # A file whose only people-shaped matches are entity keys, documentation domains or group
    # mentions names nobody. One match outside the allow-list is a finding for the whole file.
    if [ "$rule" = "people-identifier" ] &&
      ! grep -ohE "$PEOPLE_RE" "$file" | sed 's/^[[:space:]]*//' |
      grep -qvE "$PEOPLE_ALLOWED_RE"; then
      continue
    fi
    printf '%s\t%s\t%s\t%s\n' "$rule" "$file" "$dgst" "$why" >>"$work/hard.kept"
  done <"$work/hard"
  mv "$work/hard.kept" "$work/hard"
fi

# --- the baseline -----------------------------------------------------------------------

if [ "${UPDATE_BASELINE:-}" = "1" ]; then
  {
    cat <<'EOF'
# SPDX-License-Identifier: Apache-2.0
#
# Recording findings a human has looked at and accepted (scripts/check-no-secrets.sh).
#
# Each line is `rule<TAB>file<TAB>digest`. The digest is of the matched text, never the text
# itself: a baseline that quoted what it accepts would be the file the scanner exists to
# prevent. A finding not listed here fails CI, so a new secret in a new recording is a build
# failure and an accepted placeholder is one reviewed line.
#
# Regenerate with `UPDATE_BASELINE=1 scripts/check-no-secrets.sh` and review the diff: every
# added line is a human saying "I looked at this value and it is not a credential".
EOF
    sort -u <"$work/baselined" | cut -f1-3
  } >"$BASELINE_FILE"
  echo "check-no-secrets: baseline written to $BASELINE_FILE ($(grep -cv '^#' "$BASELINE_FILE" || true) entries)"
  exit 0
fi

failed=0

if [ -s "$work/hard" ]; then
  echo >&2
  echo "check-no-secrets: credentials found" >&2
  while IFS=$'\t' read -r rule file _ why; do
    printf '  %-28s %s\n      %s\n' "$rule" "$file" "$why" >&2
    failed=1
  done <"$work/hard"
fi

if [ -s "$work/baselined" ]; then
  sort -u <"$work/baselined" | while IFS=$'\t' read -r rule file dgst why; do
    if [ -f "$BASELINE_FILE" ] &&
      grep -qxF "$(printf '%s\t%s\t%s' "$rule" "$file" "$dgst")" "$BASELINE_FILE"; then
      continue
    fi
    printf '%s\t%s\t%s\t%s\n' "$rule" "$file" "$dgst" "$why"
  done >"$work/new"
  if [ -s "$work/new" ]; then
    echo >&2
    echo "check-no-secrets: new material in a recording, not in $BASELINE_FILE" >&2
    while IFS=$'\t' read -r rule file _ why; do
      printf '  %-28s %s\n      %s\n' "$rule" "$file" "$why" >&2
    done <"$work/new"
    failed=1
  fi
fi

if [ "$failed" -ne 0 ]; then
  cat >&2 <<EOF

check-no-secrets: FAILED

A recording is committed to git and stays there. If one of these is a real credential:
rotate it first, then remove it — a deleted line is still in the history.

Sanitising a recording is a human's job (fixtures/README.md §Sanitising, docs/security.md).
Keep the key names, the resourceVersion and the shape; replace every value. If you have looked
at a flagged value and it is a placeholder, add it with:

    UPDATE_BASELINE=1 $0

and explain the addition in the pull request.
EOF
  exit 1
fi

echo "check-no-secrets: clean"
