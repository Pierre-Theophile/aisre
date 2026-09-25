-- SPDX-License-Identifier: Apache-2.0
--
-- 0001_log: the append-only event log (constitution III, data-model.md "Schema log").
--
-- This schema is the source of truth. Nothing in it is ever updated or deleted; the graph
-- schema created by 0002 is a projection that can be dropped and rebuilt from here.
-- scripts/check-migrations.sh refuses any later migration that drops, deletes from or
-- truncates a log.* relation.

BEGIN;

CREATE SCHEMA IF NOT EXISTS log;

-- Feeders register themselves before they may append. `ordering` and `reordering_window`
-- are the source's declared delivery guarantees (research section 5); the projector never
-- reorders the log, it only uses them to reason about gaps.
CREATE TABLE IF NOT EXISTS log.sources (
	source_id         text PRIMARY KEY,
	kind              text NOT NULL,
	ordering          text NOT NULL DEFAULT 'none',
	reordering_window interval NOT NULL DEFAULT '0 seconds'::interval,
	schema_version    text NOT NULL,
	registered_at     timestamptz NOT NULL DEFAULT now(),
	CONSTRAINT sources_ordering_check
		CHECK (ordering IN ('per_source_sequence', 'none'))
);

COMMENT ON TABLE log.sources IS
	'Registered feeders and their declared ordering guarantees (data-model.md log.sources).';

-- The log itself. `observed_at` is assigned at append time and read back on replay, so a
-- replay reproduces the same observed-time intervals (FR-023, research section 4).
--
-- PARTITIONING (FR-016, unlimited history with no retention horizon):
-- This table is deliberately NOT declared `PARTITION BY RANGE (observed_at)` even though the
-- column layout is designed for it. PostgreSQL requires every PRIMARY KEY and UNIQUE
-- constraint of a partitioned table to contain the partition key, which would turn
-- `idempotency_key UNIQUE` into `(idempotency_key, observed_at) UNIQUE` -- a constraint that
-- no longer enforces global idempotency and therefore breaks constitution III. Correctness of
-- the idempotency key wins over a premature physical layout.
--
-- To enable monthly partitions later, once the log is large enough to justify it:
--   1. Add a migration that creates `log.events_partitioned (LIKE log.events INCLUDING ALL)`
--      declared `PARTITION BY RANGE (observed_at)`, with the PK on
--      `(event_id, observed_at)` and idempotency enforced instead by a separate,
--      non-partitioned `log.idempotency_keys (idempotency_key text PRIMARY KEY,
--      event_id text NOT NULL)` table written in the same transaction as the append.
--   2. Create one partition per month, e.g.
--        CREATE TABLE log.events_2026_09 PARTITION OF log.events_partitioned
--          FOR VALUES FROM ('2026-09-01Z') TO ('2026-10-01Z');
--   3. Copy rows across (INSERT ... SELECT, never a destructive move), then rename.
-- Step 3 is an append into a new relation; the old one stays until an ADR retires it, so the
-- append-only guard still holds.
CREATE TABLE IF NOT EXISTS log.events (
	event_id           text PRIMARY KEY,
	idempotency_key    text NOT NULL,
	source_id          text NOT NULL REFERENCES log.sources (source_id),
	source_seq         bigint,
	schema_version     text NOT NULL,
	type               text NOT NULL,
	valid_at           timestamptz,
	valid_end          timestamptz,
	valid_from_unknown boolean NOT NULL DEFAULT false,
	source_observed_at timestamptz,
	observed_at        timestamptz NOT NULL DEFAULT now(),
	principal          text,
	trace_id           text,
	payload            jsonb NOT NULL DEFAULT '{}'::jsonb,
	appended_seq       bigserial NOT NULL,
	CONSTRAINT events_idempotency_key_key UNIQUE (idempotency_key),
	CONSTRAINT events_appended_seq_key UNIQUE (appended_seq),
	-- The closed event vocabulary of data-model.md "Event types", mirroring the `body` oneof
	-- field names of api/sreagent/graph/v1/graph.proto. Adding a type is a schema change and
	-- therefore a migration plus a version bump (constitution IX).
	CONSTRAINT events_type_check CHECK (type IN (
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
		'source_checkpoint'
	))
);

COMMENT ON COLUMN log.events.observed_at IS
	'Observed-time lower bound assigned at append; replay reads it back (FR-023).';
COMMENT ON COLUMN log.events.appended_seq IS
	'Physical log order. Replay iteration only; never part of an identifier.';
COMMENT ON COLUMN log.events.principal IS
	'Authenticated individual behind a human decision event (FR-041).';

CREATE INDEX IF NOT EXISTS events_source_seq_idx
	ON log.events (source_id, source_seq);
CREATE INDEX IF NOT EXISTS events_observed_at_idx
	ON log.events (observed_at);
CREATE INDEX IF NOT EXISTS events_type_observed_at_idx
	ON log.events (type, observed_at);

-- Events refused by validation (data-model.md "Validation rules", FR-009/FR-024). Kept so an
-- operator can see what a feeder tried to send. `reason_code` vocabulary: untyped,
-- unknown_schema_version, missing_valid_time, telemetry_payload, secret_value, unknown_type,
-- ref_unresolvable.
CREATE TABLE IF NOT EXISTS log.rejected_events (
	rejected_id       bigserial PRIMARY KEY,
	event_id_or_hash  text NOT NULL,
	source_id         text,
	received_at       timestamptz NOT NULL DEFAULT now(),
	reason_code       text NOT NULL,
	reason_detail     text NOT NULL DEFAULT '',
	raw               jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS rejected_events_received_at_idx
	ON log.rejected_events (received_at);
CREATE INDEX IF NOT EXISTS rejected_events_source_id_idx
	ON log.rejected_events (source_id, received_at);

-- Re-deliveries of an already-seen idempotency key (FR-020 audit). `payload_differs` flags the
-- dangerous case: same key, different body.
CREATE TABLE IF NOT EXISTS log.duplicate_deliveries (
	duplicate_id    bigserial PRIMARY KEY,
	idempotency_key text NOT NULL,
	received_at     timestamptz NOT NULL DEFAULT now(),
	payload_differs boolean NOT NULL DEFAULT false,
	raw_hash        text NOT NULL
);

CREATE INDEX IF NOT EXISTS duplicate_deliveries_key_idx
	ON log.duplicate_deliveries (idempotency_key, received_at);
CREATE INDEX IF NOT EXISTS duplicate_deliveries_differs_idx
	ON log.duplicate_deliveries (received_at)
	WHERE payload_differs;

-- Feeder coverage declarations: "I have delivered everything between extent_from and
-- extent_to", plus an explicit gap marker (FR-052).
CREATE TABLE IF NOT EXISTS log.checkpoints (
	checkpoint_id bigserial PRIMARY KEY,
	source_id     text NOT NULL REFERENCES log.sources (source_id),
	checkpoint_at timestamptz NOT NULL DEFAULT now(),
	extent_from   timestamptz,
	extent_to     timestamptz,
	gap_before    boolean NOT NULL DEFAULT false,
	note          text NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS checkpoints_source_idx
	ON log.checkpoints (source_id, checkpoint_at DESC);

-- Least-privilege grants (research section 14). Role names are deployment-specific, so the
-- statements are templates rather than executed DDL: a deployment applies them with its own
-- role name once, after the migrations. The shape is what matters -- the application role can
-- append to and read the log, and can do nothing else to it.
--
--   CREATE ROLE sre_agent_app LOGIN;
--   GRANT USAGE ON SCHEMA log TO sre_agent_app;
--   GRANT SELECT, INSERT ON log.events            TO sre_agent_app;
--   GRANT SELECT, INSERT ON log.sources           TO sre_agent_app;
--   GRANT SELECT, INSERT ON log.rejected_events   TO sre_agent_app;
--   GRANT SELECT, INSERT ON log.duplicate_deliveries TO sre_agent_app;
--   GRANT SELECT, INSERT ON log.checkpoints       TO sre_agent_app;
--   GRANT USAGE ON ALL SEQUENCES IN SCHEMA log    TO sre_agent_app;
--   REVOKE UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA log FROM sre_agent_app;
--   ALTER DEFAULT PRIVILEGES IN SCHEMA log
--     GRANT SELECT, INSERT ON TABLES TO sre_agent_app;
--
-- The migration role is separate and owns the schema; the application role never owns it and
-- therefore cannot alter it away.

COMMIT;
