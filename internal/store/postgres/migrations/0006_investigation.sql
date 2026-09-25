-- SPDX-License-Identifier: Apache-2.0
--
-- 0006_investigation: the working material of one investigation run
-- (002 data-model.md "Schema investigation").
--
-- Numbering: this is 0006, not 0005. The feature 001 change package lands 0005_actor_kind.sql
-- (002 tasks.md T020) and migrations are numbered in the order they are applied, so 002's own
-- schema takes the next number even though it is written first. 0005 is reserved and unused
-- until that migration lands; Migrate() applies whatever is embedded in ascending version
-- order and tolerates the gap (internal/store/postgres/store.go), which the test in
-- internal/investigation/store asserts rather than assumes.
--
-- What belongs here, and what does not. Three storage locations, one rule each:
--
--   schema `log` / `graph`   the decision record, human facts, reopen links and labels, as
--                            typed events -- a fact about the production system or a decision
--                            about it;
--   schema `investigation`   THIS FILE: the run -- ledger, judgments, evidence items, worker
--                            and model calls, spend, reviews -- working material of one run;
--   files beside the graph   the recording: trajectory records, world digests, sanitised
--                            exemplars -- bulk, bounded, redacted, addressed by key + digest.
--
-- The deviation from principle I (the graph is the source of truth) is justified by
-- derivability and by nothing else: every row here is rebuildable from the event log plus the
-- recording, so dropping this schema loses no history. Nothing here is a fact about the
-- production system that is not also a copy of a graph answer carrying the `graph_event_ids`
-- it came from (002 data-model Invariant 11, plan "Complexity Tracking").
--
-- Telemetry never lands here either (constitution IV): an evidence item carries a digest, a
-- coverage block and join keys -- identifiers, parameters and aggregates -- never a sample, a
-- line or a span.
--
-- Constraints below implement 002 data-model "Invariants" 1, 2, 4, 7 and 10 explicitly; the
-- comment on each says which. The ones that cannot be a CHECK (7 and 10 read another table,
-- and 10 is a property of a set) are triggers, because "enforced by convention" is how an
-- invariant stops being one.

BEGIN;

CREATE SCHEMA IF NOT EXISTS investigation;

COMMENT ON SCHEMA investigation IS
	'Working material of investigation runs. Rebuildable from the event log plus the recording; holds no fact the graph does not (002 data-model Invariant 11).';

-- ---------------------------------------------------------------------------------------
-- Incidents and investigations
-- ---------------------------------------------------------------------------------------

-- An investigation covers an incident, not an alert (FR-008a): a second monitor firing about
-- the same subject attaches to the incident already under investigation.
CREATE TABLE IF NOT EXISTS investigation.incidents (
	incident_id               text PRIMARY KEY,
	canonical_subject_id      text NOT NULL,
	opened_at                 timestamptz NOT NULL,
	last_symptom_at           timestamptz NOT NULL,
	association_rule_version  text NOT NULL,
	status                    text NOT NULL DEFAULT 'open',
	CONSTRAINT incidents_status_check CHECK (status IN ('open', 'closed')),
	CONSTRAINT incidents_symptom_order_check CHECK (last_symptom_at >= opened_at)
);

CREATE INDEX IF NOT EXISTS incidents_subject_idx
	ON investigation.incidents (canonical_subject_id, opened_at DESC);
CREATE INDEX IF NOT EXISTS incidents_status_idx
	ON investigation.incidents (status);

-- One run. Append-only once `lifecycle` leaves 'running' (FR-007): a later run is a new row
-- linked by `reopens_investigation_id`, never an edit of this one.
CREATE TABLE IF NOT EXISTS investigation.investigations (
	investigation_id            text PRIMARY KEY,
	incident_id                 text NOT NULL REFERENCES investigation.incidents (incident_id),
	reopens_investigation_id    text REFERENCES investigation.investigations (investigation_id),
	valid_at                    timestamptz NOT NULL,
	observed_at                 timestamptz NOT NULL,
	review_mode                 boolean NOT NULL DEFAULT false,
	"window"                    tstzrange NOT NULL,
	onset_estimate_evidence_id  text,
	profile                     text NOT NULL,
	lifecycle                   text NOT NULL DEFAULT 'running',
	conclusion_kind             text,
	outcome                     text,
	stop_reason                 text,
	stop_detail                 text,
	verdict_line                text,
	rollback_candidate          text,
	provisional                 boolean NOT NULL DEFAULT true,
	requester                   text NOT NULL REFERENCES graph.principals (principal),
	model_config                jsonb NOT NULL DEFAULT '{}'::jsonb,
	algebra_version             text NOT NULL,
	ledger_rule_version         text NOT NULL,
	schema_version              text NOT NULL,
	recording_key               text,
	recording_digest            text,
	decision_event_id           text,
	started_at                  timestamptz NOT NULL,
	ended_at                    timestamptz,
	CONSTRAINT investigations_no_self_reopen
		CHECK (reopens_investigation_id IS NULL OR reopens_investigation_id <> investigation_id),
	CONSTRAINT investigations_lifecycle_check
		CHECK (lifecycle IN ('running', 'concluded', 'reopened', 'failed')),
	CONSTRAINT investigations_conclusion_kind_check
		CHECK (conclusion_kind IS NULL OR conclusion_kind IN ('partial', 'final')),
	-- A conclusion kind means a conclusion: it may not be set while the run is still going.
	CONSTRAINT investigations_conclusion_kind_requires_conclusion
		CHECK (conclusion_kind IS NULL OR lifecycle IN ('concluded', 'reopened')),
	CONSTRAINT investigations_outcome_check
		CHECK (outcome IS NULL OR outcome IN ('ranked', 'unknown', 'budget_exhausted', 'failed')),
	-- Budget exhaustion is a partial conclusion, never a final one (FR-006).
	CONSTRAINT investigations_budget_exhausted_is_partial
		CHECK (outcome IS DISTINCT FROM 'budget_exhausted' OR conclusion_kind IS DISTINCT FROM 'final'),
	CONSTRAINT investigations_window_bounds_check
		CHECK (NOT isempty("window") AND lower("window") IS NOT NULL),
	-- Invariant 7, the observed-time pin: an investigation not in review mode knows nothing
	-- learned after the instant it is about. Review mode is the explicit, recorded exception
	-- (FR-005), and it is the only way observed_at may run ahead of valid_at.
	CONSTRAINT investigations_observed_time_pin_check
		CHECK (review_mode OR observed_at <= valid_at),
	CONSTRAINT investigations_end_after_start_check
		CHECK (ended_at IS NULL OR ended_at >= started_at)
);

CREATE INDEX IF NOT EXISTS investigations_incident_idx
	ON investigation.investigations (incident_id);
CREATE INDEX IF NOT EXISTS investigations_lifecycle_idx
	ON investigation.investigations (lifecycle);
CREATE INDEX IF NOT EXISTS investigations_started_at_idx
	ON investigation.investigations (started_at DESC);
CREATE INDEX IF NOT EXISTS investigations_reopens_idx
	ON investigation.investigations (reopens_investigation_id)
	WHERE reopens_investigation_id IS NOT NULL;

COMMENT ON COLUMN investigation.investigations.review_mode IS
	'True means observed_at was "now" rather than pinned to valid_at (FR-005). Recorded, never the default.';
COMMENT ON COLUMN investigation.investigations.provisional IS
	'True while only the prior-only ranking exists -- the anytime answer an on-call sees first (FR-046a).';

-- ---------------------------------------------------------------------------------------
-- Symptoms: the two front doors, one shape
-- ---------------------------------------------------------------------------------------

-- A monitor transition and a human declaration normalise into the same row, distinguished by
-- `actor_kind` (FR-002a). `fired_at` is the instant of the transition or of the DECLARATION --
-- never the instant the engine observed it (FR-004a).
CREATE TABLE IF NOT EXISTS investigation.symptoms (
	symptom_id              text PRIMARY KEY,
	incident_id             text NOT NULL REFERENCES investigation.incidents (incident_id),
	origin_system           text NOT NULL,
	origin_ref              text NOT NULL,
	transport               text NOT NULL,
	actor_kind              text NOT NULL,
	statement               text NOT NULL DEFAULT '',
	fired_at                timestamptz NOT NULL,
	severity                text,
	title                   text,
	declaring_identity      text REFERENCES graph.principals (principal),
	idempotency_key         text NOT NULL,
	named_identifiers       jsonb NOT NULL DEFAULT '[]'::jsonb,
	target_refs             jsonb NOT NULL DEFAULT '[]'::jsonb,
	resolved_entity_ids     text[] NOT NULL DEFAULT '{}'::text[],
	resolution_evidence_id  text,
	attached_as_additional  boolean NOT NULL DEFAULT false,
	grouping_evidence_id    text,
	CONSTRAINT symptoms_idempotency_key_key UNIQUE (idempotency_key),
	CONSTRAINT symptoms_transport_check
		CHECK (transport IN ('monitor', 'human_declared', 'explicit_reference')),
	CONSTRAINT symptoms_actor_kind_check CHECK (actor_kind IN ('monitor', 'human')),
	-- A declaration is a human act by an authenticated principal, and its severity and title
	-- are as declared; neither is ever inferred, so neither may be invented by defaulting.
	CONSTRAINT symptoms_declaration_is_human
		CHECK (transport <> 'human_declared' OR actor_kind = 'human'),
	CONSTRAINT symptoms_declaration_needs_identity
		CHECK (transport <> 'human_declared' OR declaring_identity IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS symptoms_incident_idx
	ON investigation.symptoms (incident_id, fired_at);

COMMENT ON COLUMN investigation.symptoms.idempotency_key IS
	'Monitor: sha256(monitor, group_key, transition_at). Declaration: (source, stable id of the place the incident lives, declared instant) -- keyed on the stable identifier, never the name (FR-008b).';

-- FR-057f: the answer goes back to the place the incident lives -- report-only, edited in
-- place. A failed delivery is recorded and the investigation still reaches a terminal state:
-- delivery is never a precondition of concluding.
CREATE TABLE IF NOT EXISTS investigation.report_deliveries (
	delivery_id          text PRIMARY KEY,
	investigation_id     text NOT NULL REFERENCES investigation.investigations (investigation_id),
	target_system        text NOT NULL,
	target_ref           text NOT NULL,
	external_message_ref text,
	rendering_digest     text NOT NULL,
	first_delivered_at   timestamptz,
	last_updated_at      timestamptz,
	update_count         integer NOT NULL DEFAULT 0,
	outcome              text NOT NULL,
	failure_detail       text,
	CONSTRAINT report_deliveries_outcome_check
		CHECK (outcome IN ('delivered', 'failed', 'not_configured')),
	CONSTRAINT report_deliveries_failure_detail_check
		CHECK (outcome <> 'failed' OR failure_detail IS NOT NULL),
	CONSTRAINT report_deliveries_update_count_check CHECK (update_count >= 0)
);

CREATE INDEX IF NOT EXISTS report_deliveries_investigation_idx
	ON investigation.report_deliveries (investigation_id);

-- ---------------------------------------------------------------------------------------
-- Evidence: what the ledger is allowed to rest on
-- ---------------------------------------------------------------------------------------

-- Created before the hypotheses that cite it, because a judgment references both.
CREATE TABLE IF NOT EXISTS investigation.evidence_items (
	evidence_id             text PRIMARY KEY,
	investigation_id        text NOT NULL REFERENCES investigation.investigations (investigation_id),
	kind                    text NOT NULL,
	worker                  text NOT NULL DEFAULT '',
	capability              text NOT NULL DEFAULT '',
	term                    jsonb NOT NULL,
	valid_at                timestamptz NOT NULL,
	observed_at             timestamptz NOT NULL,
	called_at               timestamptz NOT NULL,
	mode                    text NOT NULL,
	outcome                 text NOT NULL,
	response_digest         text NOT NULL DEFAULT '',
	response_key            text NOT NULL DEFAULT '',
	coverage                jsonb NOT NULL,
	join_keys               jsonb NOT NULL DEFAULT '{}'::jsonb,
	graph_event_ids         text[] NOT NULL DEFAULT '{}'::text[],
	deep_link               text,
	deep_link_absent_reason text,
	truncated               boolean NOT NULL DEFAULT false,
	truncation_note         text,
	free_text               text,
	CONSTRAINT evidence_items_kind_check CHECK (kind IN (
		'algebra_answer',
		'graph_answer',
		'onset_estimate',
		'resolution_audit',
		'human_fact',
		'knowledge_item',
		'worker_failure',
		'injection_attempt',
		'verifier_finding',
		'association'
	)),
	CONSTRAINT evidence_items_mode_check CHECK (mode IN ('live', 'recorded')),
	-- Invariant 8 / FR-027: the negative outcomes are never collapsed into one another or
	-- into "no". Only `no_data` is evidence that nothing happened.
	CONSTRAINT evidence_items_outcome_check CHECK (outcome IN (
		'digest',
		'no_data',
		'not_yet_ingested',
		'query_failed',
		'not_recorded',
		'partial'
	)),
	-- Invariant 4 (FR-014a): no evidence item without a coverage block. The row is rejected,
	-- not defaulted -- a digest with no coverage is a claim about an unknown amount of data.
	CONSTRAINT evidence_items_coverage_present_check
		CHECK (jsonb_typeof(coverage) = 'object' AND coverage <> '{}'::jsonb),
	-- FR-057d: a missing deep link is explained, never silent.
	CONSTRAINT evidence_items_deep_link_check
		CHECK (deep_link IS NOT NULL OR deep_link_absent_reason IS NOT NULL),
	-- FR-037: truncation is stated in the response it applies to.
	CONSTRAINT evidence_items_truncation_note_check
		CHECK (NOT truncated OR truncation_note IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS evidence_items_investigation_called_at_idx
	ON investigation.evidence_items (investigation_id, called_at);
CREATE INDEX IF NOT EXISTS evidence_items_response_digest_idx
	ON investigation.evidence_items (response_digest);

COMMENT ON COLUMN investigation.evidence_items.free_text IS
	'At most one bounded field, flagged unverified, never citable as evidence on its own (FR-014b).';

-- Invariant 7, the half a CHECK cannot see: an investigation not in review mode can hold no
-- evidence item whose observed_at is later than the investigation's own. This is the edge
-- case "a merge or correction learned after the alert" -- the engine must not know it, and
-- must say so rather than quietly using it.
CREATE OR REPLACE FUNCTION investigation.enforce_observed_time_pin()
	RETURNS trigger
	LANGUAGE plpgsql
	AS $enforce_observed_time_pin$
DECLARE
	inv_observed_at timestamptz;
	inv_review_mode boolean;
BEGIN
	SELECT observed_at, review_mode
		INTO inv_observed_at, inv_review_mode
		FROM investigation.investigations
		WHERE investigation_id = NEW.investigation_id;

	IF inv_review_mode THEN
		RETURN NEW;
	END IF;

	IF NEW.observed_at > inv_observed_at THEN
		RAISE EXCEPTION
			'evidence item % observed at % is later than investigation % observed at %; an investigation not in review mode may not know it (002 data-model Invariant 7)',
			NEW.evidence_id, NEW.observed_at, NEW.investigation_id, inv_observed_at
			USING ERRCODE = 'check_violation';
	END IF;

	RETURN NEW;
END;
$enforce_observed_time_pin$;

COMMENT ON FUNCTION investigation.enforce_observed_time_pin() IS
	'Invariant 7: outside review mode, no evidence item may be observed later than its investigation (FR-005).';

DROP TRIGGER IF EXISTS evidence_items_observed_time_pin ON investigation.evidence_items;
CREATE TRIGGER evidence_items_observed_time_pin
	BEFORE INSERT OR UPDATE ON investigation.evidence_items
	FOR EACH ROW EXECUTE FUNCTION investigation.enforce_observed_time_pin();

-- ---------------------------------------------------------------------------------------
-- The ledger
-- ---------------------------------------------------------------------------------------

-- Numeric scale is six decimals throughout, the same rounding discipline the ranking formula
-- already publishes, so a confidence recomputed on arm64 and on amd64 is the same number
-- (Invariant 1, SC-019).
CREATE TABLE IF NOT EXISTS investigation.hypotheses (
	hypothesis_id             text PRIMARY KEY,
	investigation_id          text NOT NULL REFERENCES investigation.investigations (investigation_id),
	kind                      text NOT NULL,
	statement                 text NOT NULL,
	candidate_change_entity_id text,
	target_entity_ids         text[] NOT NULL DEFAULT '{}'::text[],
	causal_role               text NOT NULL DEFAULT 'cause',
	actor_kind                text,
	prior                     numeric(9, 6) NOT NULL,
	status                    text NOT NULL DEFAULT 'proposed',
	untested_reason           text,
	confidence                numeric(9, 6) NOT NULL,
	confidence_bucket         text NOT NULL,
	widened                   boolean NOT NULL DEFAULT false,
	widened_reason            text,
	rank                      integer NOT NULL,
	rationale                 text NOT NULL DEFAULT '',
	knowledge_derived         boolean NOT NULL DEFAULT false,
	knowledge_confirmed       boolean NOT NULL DEFAULT false,
	next_query                jsonb,
	next_query_deep_link      text,
	CONSTRAINT hypotheses_kind_check
		CHECK (kind IN ('change', 'condition', 'no_observed_change')),
	CONSTRAINT hypotheses_causal_role_check
		CHECK (causal_role IN ('cause', 'candidate_effect')),
	CONSTRAINT hypotheses_status_check
		CHECK (status IN ('proposed', 'supported', 'refuted', 'inconclusive', 'untested', 'exonerated')),
	-- Invariant 1: a confidence and a prior are probabilities. The rest of the invariant --
	-- that confidence is a pure function of prior and the judgments' ln_lr -- is asserted on
	-- every write and by the property test in internal/investigation/ledger, because it is a
	-- statement about a computation, not about a row.
	CONSTRAINT hypotheses_prior_range_check CHECK (prior >= 0 AND prior <= 1),
	CONSTRAINT hypotheses_confidence_range_check CHECK (confidence >= 0 AND confidence <= 1),
	CONSTRAINT hypotheses_bucket_check CHECK (confidence_bucket IN (
		'very_low', 'low', 'moderate', 'high', 'very_high'
	)),
	CONSTRAINT hypotheses_rank_check CHECK (rank >= 1),
	-- FR-031: `untested` is never scored as refuted and never dropped, so it must say why it
	-- was not tested and what would test it.
	CONSTRAINT hypotheses_untested_reason_check
		CHECK (status <> 'untested' OR (untested_reason IS NOT NULL AND next_query IS NOT NULL)),
	-- FR-045: a widened bucket says why it was widened.
	CONSTRAINT hypotheses_widened_reason_check
		CHECK (NOT widened OR widened_reason IS NOT NULL),
	-- FR-051: a hypothesis resting only on a document is knowledge-derived and unconfirmed,
	-- and may not reach `supported`.
	CONSTRAINT hypotheses_knowledge_only_not_supported
		CHECK (NOT (knowledge_derived AND NOT knowledge_confirmed AND status = 'supported')),
	-- `change` is the only kind that names a candidate change.
	CONSTRAINT hypotheses_candidate_change_check
		CHECK (kind = 'change' OR candidate_change_entity_id IS NULL)
);

-- Invariant 2 (FR-019a): exactly one `no_observed_change` hypothesis per investigation. The
-- unique half is structural; that it is always PRESENT is the ledger's job at creation, and
-- the deferred sum trigger below fails loudly for a set that omits it and does not renormalise.
CREATE UNIQUE INDEX IF NOT EXISTS hypotheses_one_no_observed_change_per_investigation
	ON investigation.hypotheses (investigation_id)
	WHERE kind = 'no_observed_change';

CREATE INDEX IF NOT EXISTS hypotheses_investigation_rank_idx
	ON investigation.hypotheses (investigation_id, rank);

COMMENT ON COLUMN investigation.hypotheses.confidence IS
	'Computed by the published ledger rule from the prior and the judgments. Never written by a model (FR-023, reason code model_confidence).';

-- The only mechanism by which evidence moves a confidence (FR-020a).
CREATE TABLE IF NOT EXISTS investigation.judgments (
	judgment_id      text PRIMARY KEY,
	investigation_id text NOT NULL REFERENCES investigation.investigations (investigation_id),
	hypothesis_id    text NOT NULL REFERENCES investigation.hypotheses (hypothesis_id),
	evidence_id      text NOT NULL REFERENCES investigation.evidence_items (evidence_id),
	direction        text NOT NULL,
	strength         text NOT NULL,
	ln_lr            numeric(12, 6) NOT NULL,
	source           text NOT NULL,
	worker_call_id   text,
	recorded_at      timestamptz NOT NULL DEFAULT now(),
	-- Reason code `duplicate_judgment`: one evidence item may move one hypothesis once,
	-- otherwise the same fact counted twice is the same fact believed twice (FR-020a).
	CONSTRAINT judgments_one_per_pair UNIQUE (hypothesis_id, evidence_id),
	CONSTRAINT judgments_direction_check
		CHECK (direction IN ('supports', 'refutes', 'neutral')),
	CONSTRAINT judgments_strength_check
		CHECK (strength IN ('weak', 'moderate', 'strong', 'decisive')),
	CONSTRAINT judgments_source_check
		CHECK (source IN ('first_wave', 'model', 'human_fact', 'exoneration')),
	-- A neutral judgment moves nothing, and a non-neutral one must move something.
	CONSTRAINT judgments_neutral_is_inert
		CHECK ((direction = 'neutral') = (ln_lr = 0)),
	-- Supports raises the odds, refutes lowers them; the published table's reciprocal pairs.
	CONSTRAINT judgments_direction_sign_check CHECK (
		(direction = 'supports' AND ln_lr > 0)
		OR (direction = 'refutes' AND ln_lr < 0)
		OR direction = 'neutral'
	)
);

CREATE INDEX IF NOT EXISTS judgments_investigation_hypothesis_idx
	ON investigation.judgments (investigation_id, hypothesis_id);
CREATE INDEX IF NOT EXISTS judgments_evidence_idx
	ON investigation.judgments (evidence_id);

COMMENT ON COLUMN investigation.judgments.ln_lr IS
	'The published constant for (direction, strength), stored so the posterior is reproducible from the rows alone.';

-- Invariant 10: the normalised posteriors of an investigation sum to 1.0 +/- 1e-6, INCLUDING
-- `no_observed_change`. That is what makes `unknown` a computed outcome of an open hypothesis
-- space rather than a shrug (FR-019a).
--
-- It is a property of a set, so it is a DEFERRABLE INITIALLY DEFERRED constraint trigger: the
-- ledger recomputes every posterior in one transaction after each judgment, and the check runs
-- once at commit rather than after each row, which would fail on the first of them.
CREATE OR REPLACE FUNCTION investigation.enforce_posterior_normalisation()
	RETURNS trigger
	LANGUAGE plpgsql
	AS $enforce_posterior_normalisation$
DECLARE
	subject text;
	total   numeric;
BEGIN
	subject := COALESCE(NEW.investigation_id, OLD.investigation_id);

	SELECT sum(confidence) INTO total
		FROM investigation.hypotheses
		WHERE investigation_id = subject;

	-- No rows left: nothing to normalise, and nothing to complain about.
	IF total IS NULL THEN
		RETURN NULL;
	END IF;

	IF abs(total - 1) > 0.000001 THEN
		RAISE EXCEPTION
			'hypotheses of investigation % have confidences summing to %, want 1.0 +/- 1e-6 (002 data-model Invariant 10)',
			subject, total
			USING ERRCODE = 'check_violation';
	END IF;

	RETURN NULL;
END;
$enforce_posterior_normalisation$;

COMMENT ON FUNCTION investigation.enforce_posterior_normalisation() IS
	'Invariant 10: an investigation''s posteriors sum to 1.0 +/- 1e-6, including no_observed_change. Deferred to commit because the ledger renormalises the whole set at once.';

DROP TRIGGER IF EXISTS hypotheses_posteriors_sum_to_one ON investigation.hypotheses;
CREATE CONSTRAINT TRIGGER hypotheses_posteriors_sum_to_one
	AFTER INSERT OR UPDATE OR DELETE ON investigation.hypotheses
	DEFERRABLE INITIALLY DEFERRED
	FOR EACH ROW EXECUTE FUNCTION investigation.enforce_posterior_normalisation();

-- ---------------------------------------------------------------------------------------
-- The two recorded boundaries: workers and the model
-- ---------------------------------------------------------------------------------------

-- Invariant 5: worker calls are NEVER deduplicated. A repeated identical term produces a
-- second row, is counted against budget, and is answered identically -- which is why the
-- uniqueness here is on (investigation_id, seq) and not on the term key.
CREATE TABLE IF NOT EXISTS investigation.worker_calls (
	call_id                 text PRIMARY KEY,
	investigation_id        text NOT NULL REFERENCES investigation.investigations (investigation_id),
	seq                     integer NOT NULL,
	worker                  text NOT NULL,
	capability              text NOT NULL,
	term_key                text NOT NULL,
	serves_hypothesis_id    text REFERENCES investigation.hypotheses (hypothesis_id),
	discriminating_question text NOT NULL,
	mode                    text NOT NULL,
	outcome                 text NOT NULL,
	duration_ms             integer,
	cost_class              text,
	backend                 text,
	remaining_quota         jsonb,
	quota_share_used        numeric(9, 6),
	response_bytes          integer,
	truncated               boolean NOT NULL DEFAULT false,
	evidence_id             text REFERENCES investigation.evidence_items (evidence_id),
	CONSTRAINT worker_calls_seq_unique UNIQUE (investigation_id, seq),
	CONSTRAINT worker_calls_mode_check CHECK (mode IN ('live', 'recorded')),
	CONSTRAINT worker_calls_outcome_check CHECK (outcome IN (
		'answered', 'empty', 'failed', 'timed_out', 'refused_outside_algebra', 'not_recorded'
	)),
	CONSTRAINT worker_calls_cost_class_check
		CHECK (cost_class IS NULL OR cost_class IN ('cheap', 'standard', 'expensive')),
	-- FR-018a: every call says what it is meant to settle. A call serving no hypothesis is
	-- recorded as `exploratory:<reason>`, which is still an answer to the question.
	CONSTRAINT worker_calls_purpose_check
		CHECK (discriminating_question <> ''),
	CONSTRAINT worker_calls_seq_positive_check CHECK (seq >= 0)
);

CREATE INDEX IF NOT EXISTS worker_calls_investigation_seq_idx
	ON investigation.worker_calls (investigation_id, seq);
CREATE INDEX IF NOT EXISTS worker_calls_term_key_idx
	ON investigation.worker_calls (term_key);

-- The model boundary, recorded with the same discipline as the worker boundary. `seq` is
-- interleaved with worker_calls.seq in one sequence space, which is what makes the trajectory
-- a single ordered story.
CREATE TABLE IF NOT EXISTS investigation.model_calls (
	model_call_id      text PRIMARY KEY,
	investigation_id   text NOT NULL REFERENCES investigation.investigations (investigation_id),
	seq                integer NOT NULL,
	role               text NOT NULL,
	model_id           text NOT NULL,
	effort             text,
	betas              text[] NOT NULL DEFAULT '{}'::text[],
	request_digest     text NOT NULL,
	response_digest    text NOT NULL,
	input_tokens       integer NOT NULL DEFAULT 0,
	cache_write_tokens integer NOT NULL DEFAULT 0,
	cache_read_tokens  integer NOT NULL DEFAULT 0,
	output_tokens      integer NOT NULL DEFAULT 0,
	stop_reason        text,
	duration_ms        integer,
	CONSTRAINT model_calls_seq_unique UNIQUE (investigation_id, seq),
	CONSTRAINT model_calls_role_check
		CHECK (role IN ('investigator', 'verifier', 'worker_logs_label')),
	CONSTRAINT model_calls_tokens_check CHECK (
		input_tokens >= 0 AND cache_write_tokens >= 0
		AND cache_read_tokens >= 0 AND output_tokens >= 0
	)
);

CREATE INDEX IF NOT EXISTS model_calls_investigation_seq_idx
	ON investigation.model_calls (investigation_id, seq);

COMMENT ON COLUMN investigation.model_calls.stop_reason IS
	'Including `refusal`, which is handled and recorded, never silently retried.';

-- Two rows per investigation: the trajectory layer and the world layer. The bodies are files
-- beside the graph; this table is the addressed link to them (FR-035).
CREATE TABLE IF NOT EXISTS investigation.recordings (
	investigation_id         text NOT NULL REFERENCES investigation.investigations (investigation_id),
	layer                    text NOT NULL,
	root                     text NOT NULL,
	digest                   text NOT NULL,
	bytes                    bigint NOT NULL DEFAULT 0,
	term_count               integer,
	not_recorded_count       integer,
	redaction_policy_version text NOT NULL,
	CONSTRAINT recordings_pkey PRIMARY KEY (investigation_id, layer),
	CONSTRAINT recordings_layer_check CHECK (layer IN ('trajectory', 'world')),
	-- The miss rate's numerator and denominator belong to the world layer and to nothing else.
	CONSTRAINT recordings_world_counts_check CHECK (
		(layer = 'world' AND term_count IS NOT NULL AND not_recorded_count IS NOT NULL)
		OR (layer = 'trajectory' AND term_count IS NULL AND not_recorded_count IS NULL)
	),
	CONSTRAINT recordings_counts_nonnegative_check CHECK (
		(term_count IS NULL OR term_count >= 0)
		AND (not_recorded_count IS NULL OR not_recorded_count >= 0)
	)
);

-- ---------------------------------------------------------------------------------------
-- The human channel
-- ---------------------------------------------------------------------------------------

-- A fact a person pushes at a run. It never blocks it, and a contradiction with the evidence
-- is stated rather than resolved (FR-057a).
CREATE TABLE IF NOT EXISTS investigation.human_facts (
	fact_id                      text PRIMARY KEY,
	investigation_id             text NOT NULL REFERENCES investigation.investigations (investigation_id),
	kind                         text NOT NULL,
	statement                    text NOT NULL,
	entity_ids                   text[] NOT NULL DEFAULT '{}'::text[],
	concerns                     tstzrange,
	author                       text NOT NULL REFERENCES graph.principals (principal),
	submitted_at                 timestamptz NOT NULL,
	weight_class                 text NOT NULL DEFAULT 'strong',
	evidence_id                  text REFERENCES investigation.evidence_items (evidence_id),
	event_id                     text,
	contradicted_by_evidence_ids text[] NOT NULL DEFAULT '{}'::text[],
	CONSTRAINT human_facts_kind_check CHECK (kind IN (
		'manual_action', 'vendor_notice', 'known_state', 'correction', 'other'
	)),
	-- A human fact is strong evidence; it is never decisive. A person who is certain is still
	-- a person, and the ledger must be able to report a conflict with what was measured.
	CONSTRAINT human_facts_weight_class_check CHECK (weight_class = 'strong')
);

CREATE INDEX IF NOT EXISTS human_facts_investigation_idx
	ON investigation.human_facts (investigation_id, submitted_at);

-- Additive; never an edit of the investigation it reviews (FR-054, FR-007).
CREATE TABLE IF NOT EXISTS investigation.human_reviews (
	review_id             text PRIMARY KEY,
	investigation_id      text NOT NULL REFERENCES investigation.investigations (investigation_id),
	validated_root_cause  text NOT NULL,
	hypothesis_amendments jsonb NOT NULL DEFAULT '[]'::jsonb,
	rationale             text NOT NULL DEFAULT '',
	author                text NOT NULL REFERENCES graph.principals (principal),
	decided_at            timestamptz NOT NULL,
	event_id              text
);

CREATE INDEX IF NOT EXISTS human_reviews_investigation_idx
	ON investigation.human_reviews (investigation_id, decided_at);

COMMENT ON COLUMN investigation.human_reviews.validated_root_cause IS
	'A change identifier, or `unobserved`, or `not_change_induced:<category>` -- the audit''s remainder is a valid answer, not a failure to answer.';

-- The one-click "was this right?" (FR-057e).
CREATE TABLE IF NOT EXISTS investigation.labels (
	label_id         text PRIMARY KEY,
	investigation_id text NOT NULL REFERENCES investigation.investigations (investigation_id),
	was_this_right   boolean NOT NULL,
	author           text NOT NULL REFERENCES graph.principals (principal),
	labelled_at      timestamptz NOT NULL,
	event_id         text
);

CREATE INDEX IF NOT EXISTS labels_investigation_idx
	ON investigation.labels (investigation_id, labelled_at);

-- ---------------------------------------------------------------------------------------
-- Budget and verification
-- ---------------------------------------------------------------------------------------

-- Budgets are expressed per backend AND per cost class, never as a flat call count (FR-047a);
-- both `limits` and `consumed` carry every dimension, so a historical run says what it was
-- allowed to spend as well as what it spent.
CREATE TABLE IF NOT EXISTS investigation.budget_spend (
	investigation_id    text PRIMARY KEY REFERENCES investigation.investigations (investigation_id),
	profile             text NOT NULL,
	limits              jsonb NOT NULL,
	consumed            jsonb NOT NULL DEFAULT '{}'::jsonb,
	reserve_entered_at  timestamptz,
	price_table_version text NOT NULL,
	CONSTRAINT budget_spend_limits_present_check
		CHECK (jsonb_typeof(limits) = 'object' AND limits <> '{}'::jsonb)
);

COMMENT ON COLUMN investigation.budget_spend.price_table_version IS
	'The checked-in price table in force, so a historical monetary figure stays interpretable (FR-048).';

-- The fresh-context verifier's findings (FR-022a). A finding is kept whatever its verdict:
-- a claim that was removed is part of the record of how the answer was reached.
CREATE TABLE IF NOT EXISTS investigation.verifier_findings (
	finding_id        text PRIMARY KEY,
	investigation_id  text NOT NULL REFERENCES investigation.investigations (investigation_id),
	claim_ref         text NOT NULL,
	verdict           text NOT NULL,
	offending_value   text,
	cited_evidence_id text REFERENCES investigation.evidence_items (evidence_id),
	note              text,
	action_taken      text NOT NULL,
	CONSTRAINT verifier_findings_verdict_check
		CHECK (verdict IN ('supported', 'unsupported', 'number_mismatch')),
	CONSTRAINT verifier_findings_action_check
		CHECK (action_taken IN ('removed', 'demoted', 'kept'))
);

CREATE INDEX IF NOT EXISTS verifier_findings_investigation_idx
	ON investigation.verifier_findings (investigation_id);

-- ---------------------------------------------------------------------------------------
-- The coverage audit (User Story 0) -- model-free, and the source of pi_0
-- ---------------------------------------------------------------------------------------

-- The ceiling every published accuracy target is bounded by, and the incident count it rests
-- on, so a ceiling can never be quoted without the evidence behind it (FR-069a, SC-022).
CREATE TABLE IF NOT EXISTS investigation.coverage_audits (
	audit_id       text PRIMARY KEY,
	run_at         timestamptz NOT NULL,
	incident_count integer NOT NULL,
	ceiling        numeric(9, 6) NOT NULL,
	feeder_set     jsonb NOT NULL,
	input_digest   text NOT NULL,
	author         text NOT NULL REFERENCES graph.principals (principal),
	CONSTRAINT coverage_audits_ceiling_range_check CHECK (ceiling >= 0 AND ceiling <= 1),
	CONSTRAINT coverage_audits_incident_count_check CHECK (incident_count >= 0)
);

CREATE INDEX IF NOT EXISTS coverage_audits_run_at_idx
	ON investigation.coverage_audits (run_at DESC);

COMMENT ON COLUMN investigation.coverage_audits.ceiling IS
	'The fraction of the audited incidents whose cause the graph could see. pi_0 = 1 - ceiling is the prior of the `no observed change explains this` hypothesis (ADR-0005 D9).';

-- One row per audited incident. The audit MEASURES; it never infers -- a list that supplies no
-- cause is refused rather than guessed at, so `stated_cause` is NOT NULL (FR-069).
CREATE TABLE IF NOT EXISTS investigation.coverage_audit_items (
	item_id           text PRIMARY KEY,
	audit_id          text NOT NULL REFERENCES investigation.coverage_audits (audit_id),
	incident_ref      text NOT NULL,
	alert_at          timestamptz NOT NULL,
	stated_cause      text NOT NULL,
	classification    text NOT NULL,
	category          text,
	reason            text,
	matched_entity_id text,
	CONSTRAINT coverage_audit_items_classification_check
		CHECK (classification IN ('cause_present', 'cause_absent', 'undecidable')),
	CONSTRAINT coverage_audit_items_category_check CHECK (category IS NULL OR category IN (
		'flag_flip',
		'iac_apply',
		'db_migration',
		'certificate_expiry',
		'scheduled_job',
		'third_party_outage',
		'traffic_shift',
		'latent_bug',
		'client_side_configuration',
		'business_data_change',
		'credential_leak',
		'other'
	)),
	-- An undecidable classification says why; otherwise "undecidable" is indistinguishable
	-- from "nobody looked".
	CONSTRAINT coverage_audit_items_undecidable_reason_check
		CHECK (classification <> 'undecidable' OR reason IS NOT NULL),
	CONSTRAINT coverage_audit_items_unique_incident UNIQUE (audit_id, incident_ref)
);

CREATE INDEX IF NOT EXISTS coverage_audit_items_audit_idx
	ON investigation.coverage_audit_items (audit_id);

-- ---------------------------------------------------------------------------------------
-- Least-privilege grants (research section 14, constitution VII)
--
-- Role names are deployment-specific, so these are templates rather than executed DDL, in the
-- same shape 0002_graph.sql already uses. The application role reads and writes rows; it holds
-- NO DDL on this schema, so it cannot add a table that escapes the constraints above, drop a
-- trigger that enforces an invariant, or alter a CHECK away. The migration role owns the
-- schema and is the only one that may change its shape.
--
--   GRANT USAGE ON SCHEMA investigation TO sre_agent_app;
--   GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA investigation TO sre_agent_app;
--   GRANT USAGE ON ALL SEQUENCES IN SCHEMA investigation TO sre_agent_app;
--   REVOKE CREATE ON SCHEMA investigation FROM sre_agent_app;   -- no DDL: no new tables
--   REVOKE ALL ON SCHEMA investigation FROM PUBLIC;
--
-- UPDATE and DELETE are granted here and are NOT granted on `graph.*_versions`, and the
-- difference is the point: this schema is the run's working state, which the ledger rewrites
-- many times a second as judgments arrive, while history is append-only. What keeps a
-- concluded investigation immutable (FR-007) is not a missing privilege but the lifecycle
-- rule, enforced in the DAO and asserted by its tests; and what makes the deviation safe is
-- that every row here is rebuildable from the event log plus the recording.
--
-- scripts/check-migrations.sh additionally refuses any later migration that drops, deletes
-- from or truncates investigation.investigations, investigation.evidence_items,
-- investigation.judgments or investigation.coverage_audits: they are the audit trail of how a
-- conclusion was reached, and of the ceiling every published target is measured against.
-- ---------------------------------------------------------------------------------------

COMMIT;
