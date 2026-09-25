-- SPDX-License-Identifier: Apache-2.0
--
-- Correlation keys: a value several entities share (004 T148).
--
-- The table `graph.identity_claims` answers "what is this thing called". Its
-- `UNIQUE (namespace, value, source_id)` is the whole point of it: one identifier from one source
-- names exactly one entity, which is what makes certain rule C1's premise — equality of an identifier
-- is evidence of sameness — sound.
--
-- This table answers a different question: "what does this thing have in common with others". A
-- monorepo run ships one commit to three services, so three changes carry the same
-- `deploy.commit_sha`; a redeploy ships it again. Those are not names and never were, and storing
-- them as names put the value on whichever entity the projector happened to process first — a graph
-- whose state depends on the order events arrived in.
--
-- So the uniqueness here includes the entity. Many entities may carry one key; one entity may not
-- carry the same key from one source twice, which keeps a redelivered event a no-op.
--
-- A correlation is never by itself a reason to merge. Rules that read it require corroboration as
-- well — C8 wants an agreed environment and a target the graph already resolved — which is why
-- nothing in this file resembles `merged_into`.

CREATE TABLE IF NOT EXISTS graph.correlation_keys (
	correlation_id text PRIMARY KEY,
	entity_id      text NOT NULL REFERENCES graph.entities (entity_id),
	namespace      text NOT NULL,
	value          text NOT NULL,
	attributes     jsonb NOT NULL DEFAULT '{}'::jsonb,
	source_id      text NOT NULL,
	observed_at    timestamptz NOT NULL,
	event_id       text NOT NULL,
	-- The entity is part of the key, which is the difference from identity_claims.
	CONSTRAINT correlation_keys_entity_ns_value_source_key
		UNIQUE (entity_id, namespace, value, source_id)
);

-- The lookup every correlating rule makes: "which entities share this value".
CREATE INDEX IF NOT EXISTS correlation_keys_ns_value_idx
	ON graph.correlation_keys (namespace, value);

-- And the reverse, for a rule or an audit asking what one entity is correlated by.
CREATE INDEX IF NOT EXISTS correlation_keys_entity_idx
	ON graph.correlation_keys (entity_id);

-- ---------------------------------------------------------------------------------------------
-- The re-trigger queue moves with them.
--
-- graph.pending_resolution (0011) exists because C8's answer depends on the graph and can change
-- after its evidence is stored: a change attaches to a target that did not exist when the key
-- arrived, or two target identities merge. It keyed on `claim_id` because C8 keyed on a claim.
-- C8 now keys on a correlation, so every row this queue could ever hold is a correlation id.
--
-- It is recreated rather than altered, and rather than left holding both kinds. The queue is a WORK
-- QUEUE and not history — 0011 says so in as many words, which is why it is not one of the relations
-- scripts/check-migrations.sh protects — so there is nothing here to preserve. Any row in flight at
-- the moment of this
-- migration names a deploy claim that the same feature has just stopped storing: keeping it would
-- queue work whose subject no longer exists, and the drain would spin on a row it can never resolve.
--
-- A column named for what it holds is also the point. `claim_id` holding a correlation id is how a
-- reader of this schema comes to believe an identity claim and a correlation are the same thing --
-- which is the belief this whole migration exists to take out of the system.
DROP TABLE IF EXISTS graph.pending_resolution;

CREATE TABLE graph.pending_resolution (
	-- The correlation key to re-evaluate. The primary key, so queuing the same work twice is one
	-- row: several merges in one transaction routinely touch the same change.
	correlation_id text PRIMARY KEY,
	-- Why it was queued, in words, so an operator reading the queue can tell a late attachment from
	-- a target merge without reconstructing it.
	reason         text NOT NULL,
	-- The event whose application queued it, so the work is traceable to a cause.
	event_id       text NOT NULL,
	queued_at      timestamptz NOT NULL
);

COMMENT ON TABLE graph.pending_resolution IS
	'Correlation keys whose rules must be re-evaluated because the graph moved under them: a change '
	'attached to a new target, or two target identities merged. A work queue, not history -- rows are '
	'deleted as they are drained, and every merge they cause is recorded in graph.resolution_decisions.';

-- ---------------------------------------------------------------------------------------------
-- And the new event type. Replaced rather than relaxed, as 0005, 0009 and 0010 did: the whole list is
-- restated so that one place in the migrations always says what an event may be.
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
	'correlate_entity'
));
