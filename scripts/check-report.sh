#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The metric gate (T081, SC-003, SC-004, SC-005, SC-007, SC-011, constitution V/VIII).
#
# 'aisre fixture verify --report --report-json <file>' writes one canonical JSON report per
# fixture, one document per line. That a fixture *verified* is already a hard failure of the
# command itself; this script gates on what the report says about the numbers, which an exit
# code cannot carry:
#
#   1. every fixture passed, and every step of it passed;
#   2. every rollout-regression fixture measured a ranking, and it put the true culprit inside
#      that fixture's own expected top-k (SC-005). A fixture of that family with no ranking has
#      lost its ground truth, and "no ranking" must not read as "nothing to check";
#   3. probable_auto_merges is 0 - a probable rule that merged on its own is a violation of the
#      resolution contract, not a tuning problem (FR-037, constitution VI);
#   4. certain.precision is 1.0 - a certain rule may miss a merge; it may not make a wrong one;
#   5. human_decisions_survive_replay is true (SC-007, FR-040);
#   6. every fixture compared at least one golden, and
#   7. every fixture ran at least one shuffled permutation (003 T176).
#
# 6 and 7 are there because both holes verify GREEN. A fixture with no goldens reports "0 goldens
# match" and passes; a fixture that permutes nothing reports a pass for the step that was meant to
# find order dependence. Both happened while feature 003 was being written — `fixture record` ran
# before a regeneration rather than after it — and a corpus that looked complete was asserting
# nothing about its own answers.
#
# A metric that was not measured is neither a pass nor a failure: certain.precision is null when
# no labelled pair was decided, and that is reported as "not measured" rather than counted as 0.
# What *is* a failure is a metric that is present and wrong, and - for the file as a whole - no
# fixtures at all, because an empty report has nothing to say and would otherwise be green.
#
# The investigation gate (002 FR-060, T112) is the same script under --investigation, but it
# reads a different stream: the {metric, scope, fixture, value, n, detail} rows that
# `aisre eval run --report-json` writes. Every gate below is in force from the first run
# EXCEPT the aggregate pass@1 threshold, which is set by the first full corpus run and until then
# prints as `unset` without passing. See investigation_gate() below.
#
# Usage: scripts/check-report.sh <report.jsonl>
#        scripts/check-report.sh --investigation [<rows.jsonl>]
#        scripts/check-report.sh --investigation --set-threshold <rows.jsonl> [--force]
#        scripts/check-report.sh --ceiling-guard [<targets.json> [<audit.json>]]
# Exit:  0 when every assertion held, 1 otherwise, 2 when it could not be run at all.

set -euo pipefail

# --------------------------------------------------------------------------------------------
# The investigation gate (002 FR-060, FR-061, FR-061a, FR-071; SC-001, SC-016, SC-017, SC-018,
# SC-021; tasks.md T112).
#
# `aisre eval run` writes one metric row per line — {metric, scope, fixture, value, n,
# detail} — and this is what decides whether those numbers are good enough to merge.
#
# THE ONE THRESHOLD THAT IS NOT SET YET. The aggregate pass@1 threshold is set by the **first
# full corpus run and not before** (FR-060): it cannot be guessed ahead of the corpus that
# measures it, and setting it from a partial corpus would be inventing the number the gate exists
# to defend. Until then it is read as `null` from docs/evaluation/thresholds.json, printed as
# `unset` in the table, in the job summary and on a machine-readable line, and — this is the
# point — it does **not** pass as though it had been met. It is neither a pass nor a failure; it
# is an unset gate, and saying so is the whole job.
#
# EVERY OTHER GATE IS IN FORCE FROM THE FIRST RUN:
#   * lift over the prior ≤ 0                      — a reasoning layer that does not beat the
#                                                    deterministic ranker is not worth its cost;
#   * harm rate > 5 %                              — FR-061a's definition: the prior put the
#                                                    culprit first and the investigation did not;
#   * confidently wrong > 5 %                      — a non-culprit named at `high`/`very_high`;
#   * citation validity < 100 %                    — a hard gate, and not ceiling-bounded;
#   * any untraceable conclusion;
#   * any improvised replay (a `not_recorded` served as if recorded) or any replay divergence;
#   * any metamorphic verdict change;
#   * any regression on a human-labelled case.
#
# REPORTED, NEVER GATED: the culprit's rank, top-k, calibration, cost and every timing. They are
# printed in their own table so that nobody has to guess which side of the line a number is on.
#
# NEVER GATED AT ALL: best-of-k (FR-060). It is printed with that sentence beside it.
#
# A FIXTURE'S ADMISSION, NOT THE ENGINE'S SCORE: a fixture whose world-replay `not_recorded` miss
# rate exceeds its own threshold is excluded from the corpus aggregates and listed by name. That
# is a defect in the recording, and scoring the engine on it would report the wrong thing.
#
# DETECTION POWER. A threshold is only a gate if it says what regression it can actually detect.
# Every gate below prints that sentence, computed at the run's own n (fixtures × runs, as
# Bernoulli trials) by `aisre eval power` — normal approximation to the binomial, one-sided,
# α = 0.05, power 0.8 — which is the same arithmetic `internal/eval/metrics.go` publishes, so the
# script and the report cannot drift. The method is named in every line it prints. With no
# binary and no Go toolchain the power line reads `power: unavailable` rather than being omitted:
# a missing statement of power must not look like a strong one.
#
# CEILING. Once set, the pass@1 threshold may not exceed the coverage ceiling of the audit it
# cites (FR-071); that check is here, and `ceiling_guard` below enforces the same rule over the
# published-targets file.
INVESTIGATION_THRESHOLDS=${INVESTIGATION_THRESHOLDS:-docs/evaluation/thresholds.json}

# Machine-readable lines are prefixed with this and are parsed by eval.yml's release statement.
# Shape: `gate <metric> value=<v|na> threshold=<t|unset> status=<pass|fail|unset|reported> n=<n>`.
gate_line() {
	printf 'gate %s value=%s threshold=%s status=%s n=%s\n' "$1" "$2" "$3" "$4" "$5"
}

# assert_one_row_per_corpus_metric <report> — refuse a rows file that carries the same
# corpus-scoped metric twice.
#
# Every lookup below ends in `head -1`, which is right for a well-formed report — one corpus row
# per metric, by construction (`internal/eval.corpusRows`) — and silently wrong for a file that
# is two reports catted together. Two `pass_at_1` rows would gate on whichever run happened to be
# written first, so a red run pasted after a green one would merge, and the table would print the
# other one's n beside it. There is no reading of such a file that is safe to guess at.
#
# The check is here rather than in each lookup so that the refusal names every duplicated metric
# at once, including the ones this gate does not read.
assert_one_row_per_corpus_metric() {
	duplicates=$(jq -r 'select(.scope == "corpus") | .metric' "$1" | sort | uniq -d | paste -sd, -)
	if [ -n "$duplicates" ]; then
		echo "check-report: $1 carries more than one corpus row for: ${duplicates}." >&2
		echo 'check-report: concatenated reports are not a report; run eval once or merge rows upstream.' >&2
		echo "::error title=Duplicate corpus rows::${1} carries more than one corpus row for ${duplicates}. Concatenated reports are not a report; run eval once or merge rows upstream."
		exit 1
	fi
}

# row_value <report> <metric> <scope> [fixture] — the row's value, or the empty string when the
# row is absent or its value is null. An absent measurement is never a zero.
row_value() {
	jq -r --arg m "$2" --arg s "$3" --arg f "${4:-}" '
		select(.metric == $m and .scope == $s and ((.fixture // "") == $f))
		| if .value == null then empty else (.value | tostring) end' "$1" | head -1
}

# row_n <report> <metric> <scope> [fixture] — the row's n, or 0.
row_n() {
	value=$(jq -r --arg m "$2" --arg s "$3" --arg f "${4:-}" '
		select(.metric == $m and .scope == $s and ((.fixture // "") == $f)) | (.n // 0) | tostring' "$1" | head -1)
	echo "${value:-0}"
}

# row_detail <report> <metric> <scope> [fixture] — the row's explanatory sentence.
row_detail() {
	jq -r --arg m "$2" --arg s "$3" --arg f "${4:-}" '
		select(.metric == $m and .scope == $s and ((.fixture // "") == $f)) | (.detail // "")' "$1" | head -1
}

# float_lt / float_gt — numeric comparison without bash arithmetic, which is integer-only.
float_lt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 < b + 0) }'; }
float_gt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 > b + 0) }'; }

# power_line <n> <baseline> [direction] — the detection-power sentence for a rate gate at this
# baseline. `direction` is `min` when a regression is a FALL (pass@1, citation validity) and
# `max` when it is a RISE (harm rate, confidently wrong); a statement that assumed the first for
# a max gate would say "no regression is detectable" of every threshold of the second kind.
power_line() {
	agent=$(sre_agent_bin) || {
		echo 'power: unavailable (no sre-agent binary and no go toolchain); this gate is printed without its detection power'
		return 0
	}
	# shellcheck disable=SC2086 # $agent may be `go run ./cmd/aisre`.
	$agent eval power --n "$1" --baseline "$2" --direction "${3:-min}" --quiet 2>/dev/null ||
		echo 'power: unavailable (aisre eval power failed)'
}

# zero_power_line <n> — the detection-power sentence for a gate whose threshold is zero.
zero_power_line() {
	agent=$(sre_agent_bin) || {
		echo 'power: unavailable (no sre-agent binary and no go toolchain)'
		return 0
	}
	# shellcheck disable=SC2086
	$agent eval power --n "$1" --zero-tolerance --quiet 2>/dev/null ||
		echo 'power: unavailable (aisre eval power failed)'
}

# check_rate <metric> <label> <value> <threshold> <direction> <n> — one gated rate.
#
# The metric name and the printed label are separate arguments on purpose: the table is read by a
# person and says "harm rate", the machine-readable line is read by eval.yml's release statement
# and says `harm_rate`, and a single string doing both jobs would eventually be grepped for the
# wrong one. `direction` is `min` (fail below the threshold) or `max` (fail above it).
investigation_failures=0
check_rate() {
	metric=$1 name=$2 value=$3 threshold=$4 direction=$5 n=$6
	if [ -z "$value" ]; then
		printf '  %-30s %-12s %-10s %s\n' "$name" "$threshold" 'NOT MEASURED' \
			'the run published no value for this gate'
		gate_line "$metric" na "$threshold" fail "$n"
		echo "FAIL $name was not measured; a gated metric that was not measured is not a metric that passed" >&2
		investigation_failures=$((investigation_failures + 1))
		return 0
	fi
	status=pass
	if [ "$direction" = min ] && float_lt "$value" "$threshold"; then status=fail; fi
	if [ "$direction" = max ] && float_gt "$value" "$threshold"; then status=fail; fi
	printf '  %-30s %-12s %-10s %s\n' "$name" "$threshold" "$(printf '%s' "$status" | tr '[:lower:]' '[:upper:]')" "value $value over n=$n"
	gate_line "$metric" "$value" "$threshold" "$status" "$n"
	if [ "$status" = fail ]; then
		echo "FAIL $name = $value, want $direction $threshold" >&2
		investigation_failures=$((investigation_failures + 1))
	fi
}

# check_zero <metric> <label> <value> <n> — a gate whose only acceptable value is 0.
check_zero() {
	metric=$1 name=$2 value=$3 n=$4
	if [ -z "$value" ]; then
		printf '  %-30s %-12s %-10s %s\n' "$name" '0' 'NOT MEASURED' 'the run published no value'
		gate_line "$metric" na 0 fail "$n"
		echo "FAIL $name was not measured; a zero-tolerance gate that was not measured did not hold" >&2
		investigation_failures=$((investigation_failures + 1))
		return 0
	fi
	status=pass
	if float_gt "$value" 0; then status=fail; fi
	printf '  %-30s %-12s %-10s %s\n' "$name" '0' "$(printf '%s' "$status" | tr '[:lower:]' '[:upper:]')" "value $value over n=$n"
	gate_line "$metric" "$value" 0 "$status" "$n"
	if [ "$status" = fail ]; then
		echo "FAIL $name = $value, want 0" >&2
		investigation_failures=$((investigation_failures + 1))
	fi
}

investigation_gate() {
	report=${1:-}

	if ! command -v jq >/dev/null 2>&1; then
		echo 'check-report: jq is required' >&2
		exit 2
	fi
	if [ -z "$report" ] || [ ! -s "$report" ]; then
		echo "check-report: no investigation report rows at '${report:-<none>}'" >&2
		echo '::error title=No investigation rows::The investigation gate was handed no rows. A gate with nothing to read is not a gate that passed (002 FR-060).'
		exit 1
	fi
	if ! jq -e 'has("metric")' "$report" >/dev/null 2>&1; then
		echo "check-report: $report holds no metric rows; expected the {metric, scope, fixture, value, n, detail} JSONL that 'aisre eval run --report-json' writes" >&2
		exit 1
	fi
	assert_one_row_per_corpus_metric "$report"

	trials=$(row_value "$report" trials corpus)
	trials=${trials%.*}
	trials=${trials:-0}
	fixtures=$(row_value "$report" fixtures corpus)
	fixtures=${fixtures%.*}

	echo 'check-report: investigation gate (002 FR-060, T112)'
	echo
	echo "  corpus: $(row_detail "$report" corpus corpus)"
	echo "  model:  $(row_detail "$report" model_configuration corpus)"
	echo "  n:      ${trials} Bernoulli trials (${fixtures:-0} fixtures x runs, admitted fixtures only)"
	echo

	# ---- the one threshold that is set by the first full corpus run -------------------------
	published=null
	set_by_run=null
	if [ -s "$INVESTIGATION_THRESHOLDS" ]; then
		published=$(jq -r '.pass_at_1 // "null" | tostring' "$INVESTIGATION_THRESHOLDS")
		set_by_run=$(jq -r '.set_by_run // "null" | tostring' "$INVESTIGATION_THRESHOLDS")
	fi
	pass_at_1=$(row_value "$report" pass_at_1 corpus)
	pass_n=$(row_n "$report" pass_at_1 corpus)

	printf '  %-30s %-12s %-10s %s\n' 'metric' 'threshold' 'status' 'detail'
	printf '  %-30s %-12s %-10s %s\n' '------------------------------' '------------' '----------' '------------------------------'

	if [ "$published" = null ]; then
		printf '  %-30s %-12s %-10s %s\n' 'aggregate pass@1' 'unset' 'UNSET' \
			"measured ${pass_at_1:-n/a} over n=${pass_n}; the threshold is set by the first full corpus run (FR-060)"
		gate_line pass_at_1 "${pass_at_1:-na}" unset unset "$pass_n"
		echo '::warning title=Investigation pass@1 threshold unset::The aggregate pass@1 threshold is unset and is NOT gating this build. It is set by the first full corpus run (002 FR-060, tasks.md T112) with `scripts/check-report.sh --investigation --set-threshold <report.jsonl>`, and must not exceed the coverage ceiling of the audit it cites (FR-071).'
	else
		check_rate pass_at_1 'aggregate pass@1' "$pass_at_1" "$published" min "$pass_n"
		ceiling=$(jq -r '.ceiling // 1' "$CEILING_AUDIT" 2>/dev/null || echo 1)
		if float_gt "$published" "$ceiling"; then
			echo "FAIL aggregate pass@1 threshold $published exceeds the coverage ceiling $ceiling of audit $CEILING_AUDIT (FR-071)" >&2
			investigation_failures=$((investigation_failures + 1))
		fi
	fi

	# ---- the gates in force from the first run ----------------------------------------------
	lift=$(row_value "$report" lift_over_prior corpus)
	lift_n=$(row_n "$report" lift_over_prior corpus)
	if [ -z "$lift" ]; then
		check_rate lift_over_prior 'lift over the prior' '' '>0' min "$lift_n"
	else
		status=pass
		float_gt "$lift" 0 || status=fail
		printf '  %-30s %-12s %-10s %s\n' 'lift over the prior' '> 0' "$(printf '%s' "$status" | tr '[:lower:]' '[:upper:]')" \
			"investigator MRR - prior MRR = $lift over n=$lift_n"
		gate_line lift_over_prior "$lift" 0 "$status" "$lift_n"
		if [ "$status" = fail ]; then
			echo "FAIL lift over the prior = $lift, want > 0: the reasoning layer did not beat the deterministic ranker" >&2
			investigation_failures=$((investigation_failures + 1))
		fi
	fi

	harm_max=$(jq -r '.harm_rate_max // 0.05' "$INVESTIGATION_THRESHOLDS" 2>/dev/null || echo 0.05)
	check_rate harm_rate 'harm rate' "$(row_value "$report" harm_rate corpus)" "$harm_max" max "$(row_n "$report" harm_rate corpus)"
	wrong_max=$(jq -r '.confidently_wrong_max // 0.05' "$INVESTIGATION_THRESHOLDS" 2>/dev/null || echo 0.05)
	check_rate confidently_wrong 'confidently wrong' "$(row_value "$report" confidently_wrong corpus)" "$wrong_max" max "$(row_n "$report" confidently_wrong corpus)"
	validity_min=$(jq -r '.citation_validity_min // 1.0' "$INVESTIGATION_THRESHOLDS" 2>/dev/null || echo 1.0)
	check_rate citation_validity 'citation validity' "$(row_value "$report" citation_validity corpus)" "$validity_min" min "$(row_n "$report" citation_validity corpus)"

	check_zero untraceable_conclusions 'untraceable conclusions' "$(row_value "$report" untraceable_conclusions corpus)" "$(row_n "$report" untraceable_conclusions corpus)"
	check_zero improvised_replays 'improvised replays' "$(row_value "$report" improvised_replays corpus)" "$(row_n "$report" improvised_replays corpus)"
	check_zero replay_divergence_rate 'replay divergence rate' "$(row_value "$report" replay_divergence_rate corpus)" "$(row_n "$report" replay_divergence_rate corpus)"
	# The metamorphic gate is zero-tolerance, but a run that checked no variant did not hold it:
	# it held it over nothing. That is printed as NOT RUN with an annotation, never as a pass —
	# the same rule the unset pass@1 threshold follows, for the same reason.
	metamorphic_n=$(row_n "$report" metamorphic_verdict_changes corpus)
	if [ "$metamorphic_n" = 0 ]; then
		printf '  %-30s %-12s %-10s %s\n' 'metamorphic verdict changes' '0' 'NOT RUN' \
			"$(row_detail "$report" metamorphic_verdict_changes corpus)"
		gate_line metamorphic_verdict_changes 0 0 not_run 0
		echo '::warning title=Metamorphic invariance not checked::No metamorphic variant was run, so the invariance gate held over nothing. `aisre fixture derive` generates the variants (FR-062a, SC-020); they are not checked in.'
	else
		check_zero metamorphic_verdict_changes 'metamorphic verdict changes' "$(row_value "$report" metamorphic_verdict_changes corpus)" "$metamorphic_n"
	fi
	check_zero human_label_regressions 'human-label regressions' "$(row_value "$report" human_label_regressions corpus)" "$(row_n "$report" human_label_regressions corpus)"
	echo

	# ---- detection power, per gate, at this run's own n --------------------------------------
	echo '  detection power at this n (what each gate can actually catch):'
	baseline=$published
	[ "$baseline" = null ] && baseline=${pass_at_1:-0.9}
	echo "    aggregate pass@1            $(power_line "$trials" "$baseline")"
	echo "    harm rate                   $(power_line "$trials" "$harm_max" max)"
	echo "    confidently wrong           $(power_line "$trials" "$wrong_max" max)"
	echo "    citation validity           $(zero_power_line "$trials") [a 100 % gate fires on the first invalid citation]"
	echo "    zero-tolerance gates        $(zero_power_line "$trials")"
	echo

	# ---- reported, never gated ---------------------------------------------------------------
	echo '  reported, never gated:'
	for metric in pass_hat_k culprit_rank top_k_hit_rate localisation attribution mechanism \
		unknown_rate non_unknown_precision human_agreement cost_usd worker_calls \
		time_to_provisional_seconds time_to_first_tested_seconds time_to_conclusion_seconds; do
		value=$(row_value "$report" "$metric" corpus)
		printf '    %-32s %s (n=%s)\n' "$metric" "${value:-n/a}" "$(row_n "$report" "$metric" corpus)"
		gate_line "$metric" "${value:-na}" none reported "$(row_n "$report" "$metric" corpus)"
	done
	best=$(row_value "$report" best_of_k corpus)
	printf '    %-32s %s — NEVER gated (FR-060)\n' best_of_k "${best:-n/a}"
	gate_line best_of_k "${best:-na}" never reported "$(row_n "$report" best_of_k corpus)"
	echo

	# ---- a fixture's admission, never the engine's score --------------------------------------
	excluded=$(jq -r 'select(.metric == "fixture_admitted" and .scope == "fixture" and .value == 0)
		| "    " + .fixture + ": " + .detail' "$report")
	if [ -n "$excluded" ]; then
		echo '  fixtures EXCLUDED on their `not_recorded` miss rate (this gates the fixture, never the engine):'
		printf '%s\n' "$excluded"
		gate_line fixtures_excluded "$(printf '%s\n' "$excluded" | grep -c .)" none reported "${fixtures:-0}"
	else
		echo '  every fixture was admitted: no world-replay miss rate exceeded its threshold'
		gate_line fixtures_excluded 0 none reported "${fixtures:-0}"
	fi
	echo

	# ---- the corpus miss rate, and where the recording is thin (003 T112, FR-108) -------------
	#
	# Reported, never gated, and computed over EVERY run rather than only the admitted ones: the
	# fixtures that drag this number up are exactly the ones admission removes, so a rate taken
	# after exclusion would pass by construction and say nothing.
	miss=$(row_value "$report" not_recorded_miss_rate corpus)
	if [ -n "$miss" ]; then
		echo "  world-replay miss rate (corpus): $miss — $(row_detail "$report" not_recorded_miss_rate corpus)"
		gate_line not_recorded_miss_rate "$miss" none reported "$(row_n "$report" not_recorded_miss_rate corpus)"
		jq -r 'select(.scope == "corpus" and (.metric | startswith("not_recorded_miss_rate.")))
			| "    " + (.metric | sub("^not_recorded_miss_rate."; "")) + ": " + (.value | tostring) + " not_recorded"' "$report"
	else
		echo '  world-replay miss rate (corpus): n/a — no in-algebra term was asked of a recorded world'
		gate_line not_recorded_miss_rate na none reported 0
	fi
	echo

	# ---- the corpus gap line (FR-071b) --------------------------------------------------------
	gaps=$(row_value "$report" corpus_gaps corpus)
	echo "  corpus gaps: ${gaps:-n/a} — $(row_detail "$report" corpus_gaps corpus)"
	gate_line corpus_gaps "${gaps:-na}" none reported "$(row_n "$report" corpus_gaps corpus)"
	echo

	# Whatever the thresholds are, none of them may exceed the measured ceiling (FR-071).
	ceiling_guard || exit $?

	if [ "$investigation_failures" -gt 0 ]; then
		echo "check-report: $investigation_failures investigation gate(s) failed" >&2
		exit 1
	fi
	if [ "$published" = null ]; then
		echo 'check-report: every gate in force held; the aggregate pass@1 threshold is UNSET and did not gate this build'
	else
		echo 'check-report: every investigation gate held'
	fi
	exit 0
}

# --------------------------------------------------------------------------------------------
# --set-threshold: publish the aggregate pass@1 threshold from a run's own rows (FR-060, T112).
#
# This is run ONCE, BY HAND, after the first full corpus run, and never by CI. The number cannot
# be guessed ahead of the corpus that measures it, and it cannot be re-set casually afterwards:
# a threshold that moves whenever it is inconvenient is not a threshold. Re-setting an already
# published value needs --force and shows both numbers.
#
# It refuses a value above the coverage ceiling of the audit it cites (FR-071): no recall- or
# coverage-dependent target may be published above the ceiling that bounds it.
set_threshold() {
	report=$1
	force=${2:-}

	if [ ! -s "$report" ]; then
		echo "check-report: --set-threshold needs the report of a full corpus run; $report is missing or empty" >&2
		exit 2
	fi
	value=$(row_value "$report" pass_at_1 corpus)
	n=$(row_n "$report" pass_at_1 corpus)
	if [ -z "$value" ]; then
		echo 'check-report: --set-threshold: the report publishes no aggregate pass@1' >&2
		exit 1
	fi
	ceiling=$(jq -r '.ceiling // 1' "$CEILING_AUDIT")
	audit_id=$(jq -r '.audit_id // "unknown"' "$CEILING_AUDIT")
	if float_gt "$value" "$ceiling"; then
		echo "check-report: --set-threshold: measured pass@1 $value exceeds the coverage ceiling $ceiling of audit $audit_id; a ceiling-bounded target may not be published above it (FR-071)" >&2
		exit 1
	fi
	current=$(jq -r '.pass_at_1 // "null" | tostring' "$INVESTIGATION_THRESHOLDS" 2>/dev/null || echo null)
	if [ "$current" != null ] && [ "$force" != --force ]; then
		echo "check-report: --set-threshold: pass_at_1 is already published as $current. The threshold is set by the FIRST full corpus run (FR-060); pass --force to replace it, and say why in the pull request" >&2
		exit 1
	fi

	run_id=${GITHUB_RUN_ID:-local-$(date -u +%Y%m%dT%H%M%SZ)}
	corpus_detail=$(row_detail "$report" corpus corpus)
	tmp=$(mktemp)
	jq --argjson v "$value" --arg r "$run_id" --argjson c "$ceiling" --arg a "$audit_id" \
		--arg d "$corpus_detail" --argjson n "$n" \
		'.pass_at_1 = $v | .set_by_run = $r | .coverage_ceiling = $c | .audit_id = $a
		 | .set_by_corpus = $d | .set_by_trials = $n' \
		"$INVESTIGATION_THRESHOLDS" >"$tmp"
	mv "$tmp" "$INVESTIGATION_THRESHOLDS"
	echo "check-report: published pass_at_1 = $value (n=$n, run $run_id, ceiling $ceiling of audit $audit_id) to $INVESTIGATION_THRESHOLDS"
	echo 'check-report: commit this file; from the next run on, the gate is in force'
	exit 0
}

# --------------------------------------------------------------------------------------------
# The ceiling guard (002 FR-071, SC-022, tasks.md T017).
#
# The coverage audit measures how many real incidents had a cause the graph could see. That
# fraction bounds every recall- and coverage-dependent target -- and NOTHING else. This is where
# that bound is enforced on the build.
#
# The check itself lives in Go, in `aisre audit coverage guard`, because it is not a
# threshold comparison: it has to read each criterion's [ceiling-bounded] /
# [not ceiling-bounded: precision|latency|invariance|validity] annotation, cross-check it against
# the table the specification publishes, refuse a criterion carrying no annotation at all, and
# leave the unbounded ones alone rather than scaling them down to the ceiling. Re-implementing
# that in jq would be a second, divergent copy of a rule the spec states once.
#
# Both inputs are overridable, and both absences are reported rather than assumed:
#   * no audit          -> hard failure. A ceiling-bounded target with no audit is exactly what
#                          FR-071 forbids, and a missing audit must never read as "unbounded".
#   * no published targets -> reported, exit 0. Today nothing is published: the aggregate pass@1
#                          threshold is unset (T112), so there is genuinely nothing to bound. The
#                          file appears when the first target does, and the guard bites then.
CEILING_AUDIT=${CEILING_AUDIT:-docs/evaluation/coverage-audit-2026-09.json}
CEILING_TARGETS=${CEILING_TARGETS:-docs/evaluation/published-targets.json}

# sre_agent_bin prints the command that runs the binary: $SRE_AGENT_BIN, a built bin/aisre,
# or `go run` as the last resort so a developer with a checkout and no build still gets the gate.
sre_agent_bin() {
	if [ -n "${SRE_AGENT_BIN:-}" ]; then
		echo "$SRE_AGENT_BIN"
		return 0
	fi
	if [ -x bin/aisre ]; then
		echo bin/aisre
		return 0
	fi
	if command -v go >/dev/null 2>&1; then
		echo 'go run ./cmd/aisre'
		return 0
	fi
	return 1
}

ceiling_guard() {
	audit=${1:-$CEILING_AUDIT}
	targets=${2:-$CEILING_TARGETS}

	echo
	echo 'check-report: ceiling guard (002 FR-071)'

	# The rule the guard enforces, printed where it is enforced. A reader looking at a number that
	# was capped should not have to open a second file to learn why it was capped, or that the cap
	# binds the synthetic leg on purpose.
	if [ -s "$INVESTIGATION_THRESHOLDS" ]; then
		note=$(jq -r '.note // empty' "$INVESTIGATION_THRESHOLDS" 2>/dev/null || true)
		if [ -n "$note" ]; then
			printf '%s\n' "$note" | fold -s -w 96 | sed 's/^/  /'
			echo
		fi
	fi

	if [ ! -s "$audit" ]; then
		echo "check-report: ceiling guard: no coverage audit at $audit" >&2
		echo "::error title=No coverage audit::The ceiling guard found no audit at $audit. No ceiling-bounded target may be published without one (002 FR-071, SC-022)."
		return 1
	fi
	if [ ! -s "$targets" ]; then
		echo "check-report: ceiling guard: no published targets at $targets"
		echo 'check-report: nothing is ceiling-bounded yet; the first target is published by the first full corpus run (002 FR-060, T112)'
		return 0
	fi

	agent=$(sre_agent_bin) || {
		echo 'check-report: ceiling guard: no sre-agent binary and no go toolchain' >&2
		return 2
	}
	# Intentionally unquoted: $agent may be `go run ./cmd/aisre`.
	# shellcheck disable=SC2086
	$agent audit coverage guard --report "$targets" --audit "$audit"
}

if [ "${1:-}" = '--ceiling-guard' ]; then
	shift
	if [ $# -gt 2 ]; then
		echo "usage: $0 --ceiling-guard [<targets.json> [<audit.json>]]" >&2
		exit 2
	fi
	ceiling_guard "${2:-$CEILING_AUDIT}" "${1:-$CEILING_TARGETS}"
	exit $?
fi

if [ "${1:-}" = '--investigation' ]; then
	shift
	# --set-threshold is the once-only, by-hand publication of the aggregate pass@1 gate. It is
	# spelled as a flag of --investigation rather than as its own mode because it reads exactly
	# the same rows the gate does, and must never be reachable by a job that only meant to check.
	if [ "${1:-}" = '--set-threshold' ]; then
		shift
		if [ $# -lt 1 ] || [ $# -gt 2 ]; then
			echo "usage: $0 --investigation --set-threshold <rows.jsonl> [--force]" >&2
			exit 2
		fi
		if ! command -v jq >/dev/null 2>&1; then
			echo 'check-report: jq is required' >&2
			exit 2
		fi
		set_threshold "$1" "${2:-}"
	fi
	if [ $# -gt 1 ]; then
		echo "usage: $0 --investigation [<rows.jsonl>]" >&2
		exit 2
	fi
	investigation_gate "${1:-}"
fi

if [ $# -ne 1 ]; then
	echo "usage: $0 <report.jsonl>" >&2
	echo "       $0 --investigation [<rows.jsonl>]" >&2
	echo "       $0 --investigation --set-threshold <rows.jsonl> [--force]" >&2
	echo "       $0 --ceiling-guard [<targets.json> [<audit.json>]]" >&2
	exit 2
fi
report=$1

if ! command -v jq >/dev/null 2>&1; then
	echo 'check-report: jq is required' >&2
	exit 2
fi
if [ ! -s "$report" ]; then
	echo "check-report: $report is missing or empty; the verify run wrote no fixture report" >&2
	exit 1
fi

# One jq program over the whole stream. Each input line is one fixture's report; the program
# emits one "FAIL <fixture> <what>" or "note <fixture> <what>" row per finding, and the shell
# below counts the FAILs. Keeping every assertion in one program is what makes the list above
# readable as the list of things CI is allowed to be green with.
read -r -d '' program <<'JQ' || true
def id: .fixture_id // "(unnamed)";
. as $r | id as $id | [

  # 1. the fixture as a whole, and each of its steps.
  ( if $r.passed == true then empty else "FAIL " + $id + " did not verify" end ),
  ( $r.steps // [] | .[] | select(.passed != true)
      | "FAIL " + $id + " step " + (.name // "?") + ": " + (.detail // "") ),

  # 2. ranking: measured at all for the family that promises one, and the culprit inside the
  #    fixture's own expected top-k.
  ( if ($r.metrics.family // "") == "rollout-regression" and ($r.metrics.ranking // null) == null
    then "FAIL " + $id + " is a rollout-regression fixture with no ranking: "
         + "ground_truth.culprit_change is missing, so SC-005 is not measured"
    else empty end ),
  ( $r.metrics.ranking // {} | to_entries[] | select(.value.top_k_hit != true)
      | "FAIL " + $id + " ranking " + .key
        + ": culprit " + (.value.culprit_change // "?")
        + " ranked " + (.value.rank | tostring) + " of " + (.value.candidates | tostring)
        + ", expected top " + (.value.expected_top_k | tostring) ),

  # 3-5. resolution calibration, for the fixtures that measure it.
  ( $r.metrics.calibration
      | select(. != null)
      | ( select((.probable_auto_merges // 0) != 0)
            | "FAIL " + $id + " probable_auto_merges = " + (.probable_auto_merges | tostring)
              + ", want 0: a probable rule merged with no human in the loop" ),
        ( .certain.precision as $p
            | if $p == null
              then "note " + $id + " certain.precision not measured (no labelled pair was decided)"
              elif $p >= 1.0 then empty
              else "FAIL " + $id + " certain.precision = " + ($p | tostring) + ", want 1.0"
              end ),
        ( select(.human_decisions_survive_replay != true)
            | "FAIL " + $id + " human_decisions_survive_replay = "
              + (.human_decisions_survive_replay | tostring) + ", want true" ) ),

  # 6. every fixture compared at least one golden (003 T176).
  #
  # A fixture that verifies with zero goldens compared has asserted NOTHING, and it verifies
  # green: the replay step reports "0 goldens match", every other step passes, and the report
  # says `passed: true`. That is the same class of hole as a rollout-regression fixture with no
  # ranking, and it happened twice while 003 was being written — `fixture record` runs after a
  # regeneration that had wiped `golden/`, and a corpus that looked complete was asserting
  # nothing about its own answers.
  ( if ($r.metrics.queries_compared // 0) > 0 then empty
    else "FAIL " + $id + " compared 0 goldens: the fixture verified and asserted nothing about "
         + "its own answers. Add a queries: block and run `fixture record`" end ),

  # 8. no probable rule merged on its own, on ANY fixture (003 T175, SC-021).
  #
  # Assertion 3 above checks the same thing and reaches almost none of the corpus: it reads
  # `.metrics.calibration`, which `calibrationReport` returns nil for unless the fixture's family
  # is `ambiguous-identity`. Every GCP and vendor fixture therefore passed that check by not being
  # measured by it — and SC-021's first clause is a claim about the whole corpus, not about one
  # family. `decisions_by_rule` is emitted for every reported fixture, so the claim can be held
  # over every fixture from the counts the run already publishes.
  #
  # The rule-id convention is the resolution engine's: `C…` is certain and may merge on its own,
  # `P…` is probable and may only ever suggest (internal/resolution). An auto_merge carrying a
  # probable rule id is a human taken out of the loop.
  ( $r.metrics.decisions_by_rule // {} | to_entries[]
      | select(.key | startswith("auto_merge/P"))
      | select(.value > 0)
      | "FAIL " + $id + " " + .key + " = " + (.value | tostring)
        + ", want 0: a probable rule merged with no human in the loop (SC-021)" ),

  # 9. an auto_merge names the rule that made it (003 T175, SC-021).
  #
  # `auto_merge/none` is a merge the graph made and cannot attribute. SC-021 requires 100% of
  # automated merges involving a GCP or vendor claim to be explainable by `resolve why`, and the
  # command renders the rule per decision — so a decision with no rule id is unexplainable by
  # construction, whatever else is recorded beside it.
  ( $r.metrics.decisions_by_rule // {} | to_entries[]
      | select(.key == "auto_merge/none")
      | select(.value > 0)
      | "FAIL " + $id + " " + (.value | tostring) + " auto_merge decision(s) name no rule: "
        + "`resolve why` renders the rule per decision, so these are unexplainable (SC-021)" ),

  # 10-13. SC-021's two cross-source figures, per fixture (003 T175).
  #
  # These read `.metrics.cross_source`, which a fixture produces only when its manifest labels
  # cross-source pairs. A fixture that labels none is silent here and is caught by the corpus-level
  # warning below instead — the distinction matters, because "no cross-source pair in this fixture"
  # is normal and "no cross-source pair anywhere" means both figures held over nothing.
  ( $r.metrics.cross_source
      | select(. != null)
      | # 10. the 95% figure.
        ( .auto_merge_rate as $rate
            | if $rate == null
              then "FAIL " + $id + " labelled cross-source pairs and none resolved: the 95% figure "
                   + "was computed from nothing (SC-021)"
              elif $rate >= 0.95 then empty
              else "FAIL " + $id + " cross-source auto-merge rate = " + ($rate | tostring)
                   + ", want >= 0.95: entities present in both sources with an agreed environment "
                   + "must merge under a certain rule (SC-021). Not merged: "
                   + ((.not_merged // []) | join("; "))
              end ),
        # 11. 100% of those merges explainable by the audit query.
        ( select(.explainable != .auto_merged_certain)
            | "FAIL " + $id + " " + ((.auto_merged_certain - .explainable) | tostring)
              + " cross-source merge(s) are not explainable by the audit query: "
              + ((.not_explainable // []) | join("; "))
              + " - a merge nobody can explain gets undone by the next skeptical operator (SC-021)" ),
        # 12. and the other half of the same claim: what must stay apart, stayed apart.
        ( select(((.wrongly_merged // []) | length) > 0)
            | "FAIL " + $id + " merged pair(s) the fixture labelled distinct: "
              + ((.wrongly_merged // []) | join("; "))
              + " - a rule generous enough to merge everything satisfies the 95% figure alone" ),
        # 13. a labelled pair whose reference the graph never saw is a broken label, not a
        #     measurement.
        #
        #     `unresolved` shrinks the denominator every assertion below divides by, so without
        #     this a mistyped namespace in a fixture's ground truth makes the pair VANISH: the
        #     rate stays 1.0, every count stays consistent, and the pair somebody added to widen
        #     the criterion is silently not measured. That happened while C7's pair was being
        #     added — the ref said `gcp.sql.instance` and the namespace is `gcp.cloudsql.instance`
        #     — and nothing failed.
        ( select((.unresolved // 0) > 0)
            | "FAIL " + $id + " " + (.unresolved | tostring)
              + " labelled cross-source pair(s) name a reference the graph never saw: the label is "
              + "wrong, and an unresolved pair silently shrinks the denominator rather than "
              + "failing (SC-021)" ),
        # 14. the label itself has to be a cross-source pair with an agreed environment, or the
        #     figure is measured over something the criterion is not about.
        ( (.labelled_pairs - .unresolved) as $decidable
            | select($decidable > 0)
            | select(.two_sources < $decidable)
            | "FAIL " + $id + " " + (($decidable - .two_sources) | tostring)
              + " labelled cross-source pair(s) are claimed by a single source: SC-021 is about "
              + "entities present in both GCP and another source" ),
        ( (.labelled_pairs - .unresolved) as $decidable
            | select($decidable > 0)
            | select(.agreed_environment < $decidable)
            | "FAIL " + $id + " " + (($decidable - .agreed_environment) | tostring)
              + " labelled cross-source pair(s) do not state one agreed environment on both sides: "
              + "the criterion is about entities with an agreed environment" ) ),

  # 15. EVERY deploy-claim merge is explainable, labelled or not (004 SC-017, T129).
  #
  #     Clause 11 holds only over the pairs a manifest labelled, and SC-017 says "every automated
  #     merge involving a deploy claim". A merge nobody labelled is the one nobody checked.
  ( $r.metrics.deploy_merges
      | select(. != null)
      | select(.explainable != .total)
      | "FAIL " + $id + " " + ((.total - .explainable) | tostring)
        + " deploy-claim (C8) merge(s) are not explainable by the audit query: "
        + ((.not_explainable // []) | join("; ")) + " (004 SC-017)" ),

  # 7. every fixture was shuffled.
  #
  # The shuffle is the step that finds order dependence, and it is the step a fixture silently
  # loses: a manifest with no declared reordering window permutes nothing, reports a pass, and
  # the property it was supposed to check is untested.
  ( if ($r.metrics.shuffles // 0) > 0 then empty
    else "FAIL " + $id + " ran 0 shuffled permutations: order dependence is untested, and the "
         + "step reports a pass either way" end )
] | .[]
JQ

if ! findings=$(jq -r "$program" "$report" 2>&1); then
	echo "check-report: $report is not the JSONL that fixture verify --report-json writes" >&2
	printf '%s\n' "$findings" >&2
	exit 1
fi

fixtures=$(grep -c . "$report")
ranked=$(jq -s '[.[] | select((.metrics.ranking // null) != null)] | length' "$report")
calibrated=$(jq -s '[.[] | select((.metrics.calibration // null) != null)] | length' "$report")

goldens=$(jq -s '[.[] | .metrics.queries_compared // 0] | add // 0' "$report")

# SC-021's cross-source figures are only as good as the number of pairs they were measured over, and
# the honest failure here is not a bad rate but no rate at all (003 T175). Every fixture in this
# corpus was single-source until gcp-cross-source-merge-01, so "100% of cross-source merges are
# explainable" and "95% merge under a certain rule" were both satisfied by having nothing to measure.
# A metric absent everywhere reads exactly like a metric that passed, so the absence is announced.
crosssource=$(jq -s '[.[] | select((.metrics.cross_source // null) != null)] | length' "$report")
crosspairs=$(jq -s '[.[] | .metrics.cross_source.auto_merged_certain // 0] | add // 0' "$report")

echo "check-report: $fixtures fixture(s); $ranked with a ranking, $calibrated with calibration, $goldens golden(s) compared"
if [ "$crosssource" = 0 ]; then
	echo '  cross-source merges: NOT MEASURED — no fixture labels a `ground_truth.cross_source_pairs`, so SC-021'"'"'s two cross-source figures held over nothing (100% of no merges, 95% of no pairs).'
	echo '::warning title=SC-021 cross-source figures not measured::No fixture labelled a cross-source pair, so "100% of automated merges involving a GCP or vendor claim are explainable" and "at least 95% merge automatically under a certain rule" were satisfied by having nothing to measure. A single-source corpus cannot hold either figure (SC-021).'
else
	echo "  cross-source merges: $crosspairs certain auto-merge(s) across $crosssource labelled fixture(s)"
fi

# SC-017 over the whole corpus (004 T129): every C8 merge, labelled or not, and how many were explained.
deploymerges=$(jq -s '[.[] | .metrics.deploy_merges.total // 0] | add // 0' "$report")
deployexplained=$(jq -s '[.[] | .metrics.deploy_merges.explainable // 0] | add // 0' "$report")
echo "  deploy-claim merges: $deployexplained of $deploymerges C8 merge(s) explainable by the audit query"

# Which certain rules actually fired, per rule (004 T022).
#
# The count above is rule-blind, and that is how C4, C5 and C7 each stayed dead for a whole feature:
# one rule firing in one fixture makes the corpus total non-zero, and a second rule that never fires
# anywhere is invisible behind it. C8 was the fourth in that sequence, and this line is what makes the
# fifth visible on the run that introduces it rather than on the one that finally exercises it.
#
# It is a REPORT and not a gate. The gate would have to know which rules are expected to fire, and
# nothing publishes that list — a rule whose cross-source fixture has not been built yet is honest
# work in progress, while a rule whose fixture exists and whose count is zero is a defect. Printing the
# tally is what lets a reader tell those apart; asserting on it would require guessing which one it is.
byrule=$(jq -s '[.[] | .metrics.cross_source.by_rule // {} | to_entries[]]
	| group_by(.key) | map({key: .[0].key, n: ([.[].value] | add)})
	| sort_by(.key) | map("auto_merge/" + .key + "=" + (.n | tostring)) | join(" ")' "$report")
byrule=$(printf '%s' "$byrule" | tr -d '"')
if [ -n "$byrule" ]; then
	echo "  certain rules that fired: $byrule"
else
	echo '  certain rules that fired: NONE — no labelled cross-source pair resolved under any certain rule, so every rule in the registry is currently unexercised by this corpus.'
fi
if [ -n "$findings" ]; then
	printf '%s\n' "$findings" | sed 's/^/  /'
fi

failures=$(printf '%s\n' "$findings" | grep -c '^FAIL' || true)
if [ "$failures" -gt 0 ]; then
	echo "check-report: $failures assertion(s) failed" >&2
	exit 1
fi
echo 'check-report: every assertion held'
