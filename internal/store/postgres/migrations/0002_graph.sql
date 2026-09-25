-- SPDX-License-Identifier: Apache-2.0
--
-- 0002_graph: the bitemporal projection (data-model.md "Schema graph").
--
-- Every node and edge fact carries two intervals: `valid` (when the fact was true in the
-- production system) and `observed` (when we knew it). Both are half-open tstzrange `[)`.
-- An open observed upper bound (`upper_inf(observed)`) means "current knowledge".
--
-- These tables are INSERT-only with exactly one permitted mutation: closing an open observed
-- upper bound, which may only happen through graph.close_observed() below (constitution II,
-- research section 14).

BEGIN;

-- Needed so that an exclusion constraint can mix equality on a text column with range overlap
-- in a single GiST index. btree_gist is a trusted extension since PostgreSQL 13, so the
-- database owner may create it without superuser.
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE SCHEMA IF NOT EXISTS graph;

-- Canonical identity. Not versioned: an entity id, once minted, is permanent. A merge sets
-- `merged_into` on the id that was merged away and queries follow the redirect; the merged
-- entity's versions are kept as history and never deleted (constitution VI).
CREATE TABLE IF NOT EXISTS graph.entities (
	entity_id           text PRIMARY KEY,
	type                text NOT NULL,
	created_by_event_id text NOT NULL,
	merged_into         text REFERENCES graph.entities (entity_id),
	CONSTRAINT entities_no_self_merge
		CHECK (merged_into IS NULL OR merged_into <> entity_id)
);

CREATE INDEX IF NOT EXISTS entities_type_idx
	ON graph.entities (type);
CREATE INDEX IF NOT EXISTS entities_merged_into_idx
	ON graph.entities (merged_into)
	WHERE merged_into IS NOT NULL;

-- Bitemporal node versions.
--
-- Invariant (data-model.md): for one entity_id and any observed instant, the versions whose
-- `observed` contains that instant have pairwise non-overlapping `valid` ranges. Two sources
-- asserting different values for the same key at the same valid time therefore produce ONE
-- version whose props[key] lists both records and whose `conflicts` names the key -- the
-- projector never picks a winner. The exclusion constraint is what makes that structural
-- rather than a convention.
CREATE TABLE IF NOT EXISTS graph.entity_versions (
	version_id            text PRIMARY KEY,
	entity_id             text NOT NULL REFERENCES graph.entities (entity_id),
	display_name          text NOT NULL DEFAULT '',
	valid                 tstzrange NOT NULL,
	observed              tstzrange NOT NULL,
	valid_from_unknown    boolean NOT NULL DEFAULT false,
	valid_to_unknown      boolean NOT NULL DEFAULT false,
	props                 jsonb NOT NULL DEFAULT '{}'::jsonb,
	conflicts             text[] NOT NULL DEFAULT '{}'::text[],
	pointers              jsonb NOT NULL DEFAULT '[]'::jsonb,
	change                jsonb,
	produced_by_event_ids text[] NOT NULL DEFAULT '{}'::text[],
	closed_by_event_id    text,
	CONSTRAINT entity_versions_valid_nonempty CHECK (NOT isempty(valid)),
	CONSTRAINT entity_versions_observed_nonempty CHECK (NOT isempty(observed)),
	CONSTRAINT entity_versions_closed_has_bound
		CHECK (closed_by_event_id IS NULL OR NOT upper_inf(observed)),
	CONSTRAINT entity_versions_no_overlap EXCLUDE USING gist (
		entity_id WITH =,
		valid WITH &&,
		observed WITH &&
	)
);

COMMENT ON COLUMN graph.entity_versions.observed IS
	'Observed-time interval, half-open [). upper_inf() means current knowledge.';
COMMENT ON COLUMN graph.entity_versions.valid_from_unknown IS
	'The valid lower bound is a first-observation placeholder, not a known instant (FR-011).';
COMMENT ON COLUMN graph.entity_versions.props IS
	'OTel-conventional keys; each value is {value, source_id, event_id}, or a list of those when sources disagree.';

CREATE INDEX IF NOT EXISTS entity_versions_temporal_idx
	ON graph.entity_versions USING gist (valid, observed);
CREATE INDEX IF NOT EXISTS entity_versions_entity_id_idx
	ON graph.entity_versions (entity_id);
CREATE INDEX IF NOT EXISTS entity_versions_props_idx
	ON graph.entity_versions USING gin (props);
CREATE INDEX IF NOT EXISTS entity_versions_change_kind_idx
	ON graph.entity_versions ((change ->> 'kind'))
	WHERE change IS NOT NULL;

-- Bitemporal edge versions. Same rules; the exclusion key is the (src, dst, type) triple.
CREATE TABLE IF NOT EXISTS graph.edge_versions (
	version_id               text PRIMARY KEY,
	src_id                   text NOT NULL REFERENCES graph.entities (entity_id),
	dst_id                   text NOT NULL REFERENCES graph.entities (entity_id),
	type                     text NOT NULL,
	valid                    tstzrange NOT NULL,
	observed                 tstzrange NOT NULL,
	valid_from_unknown       boolean NOT NULL DEFAULT false,
	valid_to_unknown         boolean NOT NULL DEFAULT false,
	weight_class             smallint,
	props                    jsonb NOT NULL DEFAULT '{}'::jsonb,
	produced_by_event_ids    text[] NOT NULL DEFAULT '{}'::text[],
	closed_by_event_id       text,
	closed_as_consequence_of text,
	CONSTRAINT edge_versions_valid_nonempty CHECK (NOT isempty(valid)),
	CONSTRAINT edge_versions_observed_nonempty CHECK (NOT isempty(observed)),
	CONSTRAINT edge_versions_closed_has_bound
		CHECK (closed_by_event_id IS NULL OR NOT upper_inf(observed)),
	CONSTRAINT edge_versions_weight_class_range
		CHECK (weight_class IS NULL OR weight_class BETWEEN 0 AND 5),
	CONSTRAINT edge_versions_no_overlap EXCLUDE USING gist (
		src_id WITH =,
		dst_id WITH =,
		type WITH =,
		valid WITH &&,
		observed WITH &&
	)
);

COMMENT ON COLUMN graph.edge_versions.weight_class IS
	'Traffic weight class 0-5, `calls` edges only (research section 8).';
COMMENT ON COLUMN graph.edge_versions.closed_as_consequence_of IS
	'version_id of the retracted node version that cascaded this edge closure.';

CREATE INDEX IF NOT EXISTS edge_versions_temporal_idx
	ON graph.edge_versions USING gist (valid, observed);
CREATE INDEX IF NOT EXISTS edge_versions_src_id_idx
	ON graph.edge_versions (src_id);
CREATE INDEX IF NOT EXISTS edge_versions_dst_id_idx
	ON graph.edge_versions (dst_id);
CREATE INDEX IF NOT EXISTS edge_versions_type_idx
	ON graph.edge_versions (type);
CREATE INDEX IF NOT EXISTS edge_versions_props_idx
	ON graph.edge_versions USING gin (props);

-- Every identity assertion a source made, stored before any merge decision (constitution VI).
CREATE TABLE IF NOT EXISTS graph.identity_claims (
	claim_id    text PRIMARY KEY,
	entity_id   text NOT NULL REFERENCES graph.entities (entity_id),
	namespace   text NOT NULL,
	value       text NOT NULL,
	attributes  jsonb NOT NULL DEFAULT '{}'::jsonb,
	source_id   text NOT NULL,
	observed_at timestamptz NOT NULL,
	event_id    text NOT NULL,
	CONSTRAINT identity_claims_ns_value_source_key UNIQUE (namespace, value, source_id)
);

CREATE INDEX IF NOT EXISTS identity_claims_ns_value_idx
	ON graph.identity_claims (namespace, value);
CREATE INDEX IF NOT EXISTS identity_claims_entity_id_idx
	ON graph.identity_claims (entity_id);

-- Append-only history of every merge, split and human override. `superseded_by` chains a
-- decision to the one that replaced it; nothing is rewritten.
CREATE TABLE IF NOT EXISTS graph.resolution_decisions (
	decision_id          text PRIMARY KEY,
	kind                 text NOT NULL,
	surviving_id         text,
	merged_id            text,
	rule_id              text,
	score                numeric,
	rationale            text NOT NULL DEFAULT '',
	supporting_claim_ids text[] NOT NULL DEFAULT '{}'::text[],
	principal            text,
	decided_at           timestamptz NOT NULL DEFAULT now(),
	event_id             text NOT NULL,
	superseded_by        text REFERENCES graph.resolution_decisions (decision_id),
	CONSTRAINT resolution_decisions_kind_check CHECK (kind IN (
		'auto_merge',
		'suggest',
		'confirm',
		'reject',
		'split',
		'manual_merge'
	))
);

CREATE INDEX IF NOT EXISTS resolution_decisions_surviving_idx
	ON graph.resolution_decisions (surviving_id);
CREATE INDEX IF NOT EXISTS resolution_decisions_merged_idx
	ON graph.resolution_decisions (merged_id);
CREATE INDEX IF NOT EXISTS resolution_decisions_decided_at_idx
	ON graph.resolution_decisions (decided_at);

-- Current view of pending/decided candidate pairs. One row per unordered pair; the key is
-- `min(id)|max(id)` so a pair can never be queued twice under two orderings.
CREATE TABLE IF NOT EXISTS graph.suggestions (
	pair_key    text PRIMARY KEY,
	entity_a    text NOT NULL,
	entity_b    text NOT NULL,
	rule_id     text NOT NULL,
	score       numeric NOT NULL,
	rationale   text NOT NULL DEFAULT '',
	created_at  timestamptz NOT NULL DEFAULT now(),
	status      text NOT NULL DEFAULT 'pending',
	decision_id text REFERENCES graph.resolution_decisions (decision_id),
	CONSTRAINT suggestions_status_check
		CHECK (status IN ('pending', 'confirmed', 'rejected', 'conflict')),
	CONSTRAINT suggestions_pair_ordered CHECK (entity_a < entity_b),
	CONSTRAINT suggestions_pair_key_matches
		CHECK (pair_key = entity_a || '|' || entity_b)
);

CREATE INDEX IF NOT EXISTS suggestions_status_score_idx
	ON graph.suggestions (status, score DESC);

-- Authenticated individuals whose decisions the log records (FR-041).
CREATE TABLE IF NOT EXISTS graph.principals (
	principal    text PRIMARY KEY,
	email        text,
	display_name text,
	first_seen   timestamptz NOT NULL DEFAULT now(),
	last_seen    timestamptz NOT NULL DEFAULT now(),
	roles        text[] NOT NULL DEFAULT '{}'::text[]
);

-- The single permitted mutation of a version row.
--
-- A correction does not overwrite history: it closes the observed upper bound of the old
-- version and inserts a new one (constitution II). This function is that close, and nothing
-- else. It is SECURITY DEFINER so that the application role can be granted EXECUTE on it
-- while holding no UPDATE privilege on the version tables at all.
--
-- Refusals (all raise, none silently no-op):
--   * unknown version table            -> invalid_parameter_value
--   * no such version_id               -> no_data_found
--   * observed upper bound already set -> check_violation  (never reopen, never move)
--   * closed_at at or before the observed lower bound -> invalid_parameter_value
--
-- Returns the closed observed interval so the caller can record what it did.
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

COMMENT ON FUNCTION graph.close_observed(text, text, timestamptz, text) IS
	'Close an OPEN observed upper bound on a version row. The only permitted UPDATE on graph.*_versions.';

-- Least-privilege grants (research section 14). Role names are deployment-specific, so these
-- are templates rather than executed DDL. The application role inserts versions and executes
-- close_observed(); it never holds UPDATE or DELETE on a version table.
--
--   GRANT USAGE ON SCHEMA graph TO sre_agent_app;
--   GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA graph TO sre_agent_app;
--   REVOKE UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA graph FROM sre_agent_app;
--   GRANT UPDATE ON graph.entities   TO sre_agent_app;  -- merged_into redirect only
--   GRANT UPDATE ON graph.suggestions TO sre_agent_app; -- status transitions only
--   GRANT USAGE ON ALL SEQUENCES IN SCHEMA graph TO sre_agent_app;
--   REVOKE ALL ON FUNCTION graph.close_observed(text, text, timestamptz, text) FROM PUBLIC;
--   GRANT EXECUTE ON FUNCTION graph.close_observed(text, text, timestamptz, text)
--     TO sre_agent_app;
--
-- Until a deployment applies them, close_observed() stays executable by PUBLIC: that is the
-- safe default, because the function is itself the privilege boundary -- it can only ever
-- close an open bound.

COMMIT;
