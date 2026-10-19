-- SPDX-License-Identifier: Apache-2.0
--
-- 0015_catalog: the service catalog's aliases, alias proposals and attribution decisions
-- (feature 009, ADR-0015, specs/009-service-catalog/data-model.md).
--
-- Catalog entries themselves are graph nodes (SERVICE, `sre.catalog=<slug>`) and attributions are
-- ATTRIBUTED_TO edges, so they live in graph.entity_versions and graph.edge_versions like every
-- other fact. What needs tables of its own is what is not a node or an edge:
--
--   catalog_alias_versions  a NAME (namespace, value, environment) that denotes an entry. Not an
--                           identity claim and not a merge: `candidate-api` in production and in
--                           staging stay two entities, both aliased to one entry (research R2).
--                           History, like every *_versions table: rows are inserted, and the one
--                           permitted mutation is closing an open `observed` interval.
--   alias_proposals         a probable rule's suggestion that a name belongs to an entry, pending
--                           a person's decision (research R9), shaped like proposed_dependencies.
--   attribution_decisions   the audit side of attribute_signal, correct_attribution and
--                           revert_correction: what was decided, by which layer and rule, with
--                           the suggestions and evidence, so "why is this attributed?" and "why is
--                           this unknown?" are both answerable (FR-039).
--
-- All three are projections of the event log and are rebuilt by a replay from empty.

BEGIN;

CREATE TABLE IF NOT EXISTS graph.catalog_alias_versions (
	version_id          text PRIMARY KEY,
	namespace           text NOT NULL,
	value               text NOT NULL,
	-- Empty matches every environment; a stated one matches only signals stating the same.
	environment         text NOT NULL DEFAULT '',
	-- The entry's slug, or '-' for a hidden name ("not a service"), which has no node.
	entry_slug          text NOT NULL,
	observed            tstzrange NOT NULL,
	decided_by_event_id text NOT NULL,
	-- Exactly one of the two: a person, or the rule or proposal that carried the alias (FR-050).
	principal           text,
	rule_id             text,
	closed_by_event_id  text,
	CONSTRAINT catalog_alias_versions_observed_nonempty CHECK (NOT isempty(observed)),
	CONSTRAINT catalog_alias_versions_decider CHECK ((principal IS NULL) <> (rule_id IS NULL)),
	CONSTRAINT catalog_alias_versions_closed_has_bound
		CHECK (closed_by_event_id IS NULL OR NOT upper_inf(observed)),
	-- At one observed instant a name denotes at most one entry: a reassignment closes the old row.
	CONSTRAINT catalog_alias_versions_no_overlap EXCLUDE USING gist (
		namespace WITH =,
		value WITH =,
		environment WITH =,
		observed WITH &&
	)
);

CREATE INDEX IF NOT EXISTS catalog_alias_versions_name_idx
	ON graph.catalog_alias_versions (namespace, value);
CREATE INDEX IF NOT EXISTS catalog_alias_versions_entry_idx
	ON graph.catalog_alias_versions (entry_slug);

CREATE TABLE IF NOT EXISTS graph.alias_proposals (
	proposal_key          text PRIMARY KEY,
	namespace             text NOT NULL,
	value                 text NOT NULL,
	environment           text NOT NULL DEFAULT '',
	entry_slug            text NOT NULL,
	rule_id               text NOT NULL,
	score                 numeric NOT NULL,
	rationale             text NOT NULL DEFAULT '',
	evidence              jsonb NOT NULL DEFAULT '{}'::jsonb,
	created_at            timestamptz NOT NULL,
	status                text NOT NULL DEFAULT 'pending',
	proposed_by_event_ids text[] NOT NULL DEFAULT '{}'::text[],
	decided_by            text,
	decided_at            timestamptz,
	decision_event_id     text,
	CONSTRAINT alias_proposals_status_check
		CHECK (status IN ('pending', 'accepted', 'rejected', 'conflict'))
);

CREATE INDEX IF NOT EXISTS alias_proposals_status_score_idx
	ON graph.alias_proposals (status, score DESC);

CREATE TABLE IF NOT EXISTS graph.attribution_decisions (
	event_id               text PRIMARY KEY,
	-- attribute: the attributor decided. correct: a person did. revert: a person undid a
	-- correction. conflict: an automated decision that disagreed with a standing correction and was
	-- recorded without effect (FR-042).
	kind                   text NOT NULL,
	signal_entity_id       text NOT NULL REFERENCES graph.entities (entity_id),
	signal_ref             text NOT NULL,
	entries                jsonb NOT NULL DEFAULT '[]'::jsonb,
	suggestions            jsonb NOT NULL DEFAULT '[]'::jsonb,
	ruleset                text NOT NULL DEFAULT '',
	evidence               jsonb NOT NULL DEFAULT '{}'::jsonb,
	scope                  text NOT NULL DEFAULT '',
	principal              text,
	decided_at             timestamptz NOT NULL,
	superseded_by_event_id text,
	reverted_by_event_id   text,
	CONSTRAINT attribution_decisions_kind_check
		CHECK (kind IN ('attribute', 'correct', 'revert', 'conflict')),
	CONSTRAINT attribution_decisions_person
		CHECK (kind NOT IN ('correct', 'revert') OR principal IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS attribution_decisions_signal_idx
	ON graph.attribution_decisions (signal_entity_id, decided_at);

-- An alias is reassigned or retracted by closing its open interval, which is the one mutation a
-- version table permits. graph.close_observed (0002) is that mutation's only door, so it is restated
-- here with the alias table added; its body is otherwise 0002's, unchanged.
CREATE OR REPLACE FUNCTION graph.close_observed(
	version_table      text,
	version_id         text,
	closed_at          timestamptz,
	closed_by_event_id text
) RETURNS tstzrange
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $close_observed$
DECLARE
	target_table text;
	current_observed tstzrange;
	closed_observed tstzrange;
BEGIN
	target_table := CASE version_table
		WHEN 'entity_versions' THEN 'entity_versions'
		WHEN 'graph.entity_versions' THEN 'entity_versions'
		WHEN 'edge_versions' THEN 'edge_versions'
		WHEN 'graph.edge_versions' THEN 'edge_versions'
		WHEN 'catalog_alias_versions' THEN 'catalog_alias_versions'
		WHEN 'graph.catalog_alias_versions' THEN 'catalog_alias_versions'
		ELSE NULL
	END;

	IF target_table IS NULL THEN
		RAISE EXCEPTION 'close_observed: % is not a version table', version_table
			USING ERRCODE = 'invalid_parameter_value';
	END IF;

	IF closed_at IS NULL THEN
		RAISE EXCEPTION 'close_observed: closed_at must not be null'
			USING ERRCODE = 'invalid_parameter_value';
	END IF;

	EXECUTE format(
		'SELECT observed FROM graph.%I WHERE version_id = $1 FOR UPDATE', target_table
	) INTO current_observed USING version_id;

	IF current_observed IS NULL THEN
		RAISE EXCEPTION 'close_observed: no version % in graph.%', version_id, target_table
			USING ERRCODE = 'no_data_found';
	END IF;

	IF NOT upper_inf(current_observed) THEN
		RAISE EXCEPTION
			'close_observed: observed bound of % is already closed at %; history is append-only',
			version_id, upper(current_observed)
			USING ERRCODE = 'check_violation';
	END IF;

	IF closed_at <= lower(current_observed) THEN
		RAISE EXCEPTION
			'close_observed: closed_at % is not after the observed lower bound %',
			closed_at, lower(current_observed)
			USING ERRCODE = 'invalid_parameter_value';
	END IF;

	closed_observed := tstzrange(lower(current_observed), closed_at, '[)');

	EXECUTE format(
		'UPDATE graph.%I SET observed = $1, closed_by_event_id = $2 WHERE version_id = $3',
		target_table
	) USING closed_observed, closed_by_event_id, version_id;

	RETURN closed_observed;
END;
$close_observed$;

-- And the new event types. Replaced rather than relaxed, as 0005, 0009, 0010 and 0012 did: the
-- whole list is restated so that one place in the migrations always says what an event may be.
ALTER TABLE log.events DROP CONSTRAINT IF EXISTS events_type_check;

ALTER TABLE log.events ADD CONSTRAINT events_type_check CHECK (type IN (
	'upsert_node',
	'upsert_edge',
	'retract_node',
	'retract_edge',
	'observe_change',
	'identity_claim',
	'confirm_merge',
	'reject_merge',
	'split_entity',
	'manual_merge',
	'source_checkpoint',
	-- ADR-0005 D2/D3: the decision record, the human channel and alert intake.
	'record_investigation',
	'submit_human_fact',
	'reopen_investigation',
	'label_investigation',
	'alert_transition',
	-- 003 plan item 2: a suggestion that an edge exists, and the two decisions about one.
	'propose_dependency',
	'confirm_dependency',
	'reject_dependency',
	-- 004 T148: a value several entities share, which an identity claim cannot express.
	'correlate_entity',
	-- ADR-0015 (feature 009): the service catalog, its aliases, and attribution.
	'propose_catalog_entry',
	'declare_catalog_entry',
	'declare_alias',
	'retract_alias',
	'propose_alias',
	'reject_alias',
	'attribute_signal',
	'correct_attribution',
	'revert_correction'
));

COMMIT;
