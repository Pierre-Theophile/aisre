#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0
#
# Append-only guard (constitution III, research §14; 002 constitution III, FR-007).
#
# The event log and the bitemporal version tables are history. A migration may add tables,
# columns, indexes and constraints; it may never drop, delete from, or truncate the history
# itself. This refuses any migration that does.
#
# Four tables of schema `investigation` are protected on the same terms, for the same reason:
#
#   investigation.investigations    what was concluded, by which run, under which budget and
#                                   model configuration. A concluded investigation is immutable
#                                   (FR-007); a later run is a new row, never an edit.
#   investigation.evidence_items    what every claim rests on. Deleting one makes a published
#                                   conclusion unfalsifiable after the fact.
#   investigation.judgments         the only mechanism by which evidence moved a confidence.
#                                   Without them a confidence is a number nobody can recompute.
#   investigation.coverage_audits   the measured ceiling every published accuracy target is
#                                   bounded by (FR-071). A target whose audit has been deleted
#                                   is a target with no evidence behind it.
#
# The rest of schema `investigation` is working material of a run, rebuildable from the event
# log plus the recording, and is deliberately NOT protected here: this guard is about the audit
# trail of how a conclusion was reached, not about the scratch space it was reached in.
#
# Exit 0 when the migrations directory is empty or every file is clean, 1 otherwise.

set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
MIGRATIONS="$ROOT/internal/store/postgres/migrations"

if [ ! -d "$MIGRATIONS" ]; then
	echo "check-migrations: $MIGRATIONS does not exist yet, nothing to check"
	exit 0
fi

files=$(find "$MIGRATIONS" -name '*.sql' -type f | sort)
if [ -z "$files" ]; then
	echo "check-migrations: no migrations yet, nothing to check"
	exit 0
fi

# A destructive statement is DROP TABLE / DELETE FROM / TRUNCATE (TABLE) whose target is a
# protected relation: anything in the `log` schema, a `*_versions` table in `graph`, or one of
# the four investigation tables that hold the audit trail of a conclusion.
# Comments are stripped first so that documentation of the rule does not trip it.
protected='(log\.[a-z_]+|graph\.[a-z_]*_versions|investigation\.(investigations|evidence_items|judgments|coverage_audits))'
verbs='(DROP[[:space:]]+TABLE([[:space:]]+IF[[:space:]]+EXISTS)?|DELETE[[:space:]]+FROM|TRUNCATE([[:space:]]+TABLE)?)'

# Dropping the schema takes every table in it with it, so the relation-level rule above would
# be trivially evaded by one line. No migration has ever needed to, so this is a flat refusal.
schemas='(log|graph|investigation)'
schema_verb='DROP[[:space:]]+SCHEMA([[:space:]]+IF[[:space:]]+EXISTS)?'

status=0
for file in $files; do
	stripped=$(
		sed -e 's/--.*$//' "$file" |
			tr '\n' ' ' |
			tr -s '[:space:]' ' '
	)
	offenders=$(
		printf '%s' "$stripped" |
			grep -ioE "$verbs[[:space:]]+(ONLY[[:space:]]+)?$protected" || true
	)
	schema_offenders=$(
		printf '%s' "$stripped" |
			grep -ioE "$schema_verb[[:space:]]+$schemas\b" || true
	)
	if [ -n "$offenders" ] || [ -n "$schema_offenders" ]; then
		status=1
		echo "check-migrations: destructive statement on protected history in $file:"
		if [ -n "$offenders" ]; then
			echo "$offenders" | sed 's/^/  /'
		fi
		if [ -n "$schema_offenders" ]; then
			echo "$schema_offenders" | sed 's/^/  /'
		fi
	fi
done

if [ "$status" -ne 0 ]; then
	cat <<'EOF'

History is append-only. `log.*`, `graph.*_versions` and the four investigation tables that hold
the audit trail of a conclusion -- `investigation.investigations`,
`investigation.evidence_items`, `investigation.judgments`, `investigation.coverage_audits` --
may not be dropped, deleted from or truncated by a migration, and neither may the schemas that
contain them. Corrections close an observed interval and open a new version; retractions close
a valid interval (constitution II and III); a concluded investigation is superseded by a new
row that links to it, never edited (002 FR-007). If you believe this migration is the
exception, it needs an ADR under docs/decisions/ and a constitution amendment.
EOF
	exit 1
fi

echo "check-migrations: ok ($(echo "$files" | wc -l | tr -d ' ') file(s) checked)"
