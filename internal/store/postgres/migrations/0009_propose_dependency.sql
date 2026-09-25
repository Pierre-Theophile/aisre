-- SPDX-License-Identifier: Apache-2.0
--
-- 0009_propose_dependency: the sixth event body (003 FR-029, plan item 2).
--
-- log.events.type is a closed vocabulary mirroring the `body` oneof of graph.proto, and 0001 says
-- in as many words that adding a type is a migration plus a version bump. This is that migration,
-- for one addition.
--
-- What `propose_dependency` is, and why the schema needed a new body rather than reusing one.
-- Resolution can already suggest that two REFS name the same ENTITY -- that is `identity_claim`
-- and the suggestion machinery behind it. Nothing could suggest that an EDGE exists between two
-- DIFFERENT entities. So a feeder holding suggestive but inconclusive evidence of a dependency had
-- exactly two options, and both are worse than a stated gap: assert the edge anyway, or drop the
-- evidence on the floor.
--
-- The concrete case this exists for: a Cloud Run service whose configuration, connection settings
-- or labels name a Cloud SQL instance gets a `depends-on` edge with the evidence that derived it
-- recorded (003 FR-028). Where the dependency cannot be derived, the feeder proposes it instead
-- and asserts nothing.
--
-- The invariant this row must never break: **a proposal is not an edge.** It is persisted as a
-- pending suggestion, no query traverses it, and only a human confirmation promotes it. That is
-- enforced above this file -- the projector never writes a graph edge from this body -- but it is
-- stated here because this is the table where somebody would first think of shortcutting it.
--
-- The constraint is REPLACED rather than relaxed, exactly as 0005 did: an unknown type is still
-- refused, the set is simply one member larger.

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
	-- 003 plan item 2: a suggestion that an edge exists, which is never itself an edge.
	'propose_dependency'
));
