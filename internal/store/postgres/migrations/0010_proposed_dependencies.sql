-- SPDX-License-Identifier: Apache-2.0
--
-- 0010_proposed_dependencies: where a proposal lives while it is not an edge (003 FR-029, FR-122,
-- plan item 2; data-model.md §4.2 "The proposed dependency, which is not an edge").
--
-- 0009 added the `propose_dependency` event body. This adds the table it projects into, and the
-- two decision types that resolve it.
--
-- ---------------------------------------------------------------------------
-- Why this is not graph.suggestions
--
-- graph.suggestions holds the claim that two REFS name one ENTITY. That relation is symmetric, so
-- the table stores its pair ordered (`entity_a < entity_b`) and keys on the ordered pair. A
-- dependency is DIRECTED: "checkout depends on orders-db" and the reverse are different claims,
-- only one of which is true, and filing them under an ordered pair would merge them into one row
-- that means neither. So this table keys on the (src, dst, type) triple in the order asserted, and
-- the ordering constraint graph.suggestions carries is deliberately absent here.
--
-- It also holds a third column suggestions has no use for: the EVIDENCE. A reviewer of an identity
-- suggestion can look at the two entities and judge; a reviewer of a proposed dependency is being
-- asked about a relationship that is not visible from either end, so the material the rule saw has
-- to travel with the proposal (constitution V).
--
-- ---------------------------------------------------------------------------
-- The invariant
--
-- **A row here is not an edge and never becomes one by itself.** No query traverses this table, no
-- projection derives an edge from it, and `graph.edges` gains a row only when a ConfirmDependency
-- event is applied. That is enforced in the projector; it is restated here because this is the
-- table where somebody would first think of shortcutting it with a view.
--
-- `status` is therefore a small closed set, and `conflict` carries the weight FR-122 puts on it: a
-- rule that goes on proposing something a person rejected is recorded as disagreeing, never as
-- reopening the question.

CREATE TABLE IF NOT EXISTS graph.proposed_dependencies (
	proposal_key  text PRIMARY KEY,
	src_entity_id text NOT NULL,
	dst_entity_id text NOT NULL,
	edge_type     text NOT NULL,
	rule_id       text NOT NULL,
	score         numeric NOT NULL,
	rationale     text NOT NULL DEFAULT '',
	-- The material the rule matched on, so a reviewer decides from what the rule saw rather than
	-- from its score alone.
	evidence      jsonb NOT NULL DEFAULT '{}'::jsonb,
	created_at    timestamptz NOT NULL,
	status        text NOT NULL DEFAULT 'pending',

	-- Both halves of the audit trail. The proposal side accumulates: a rule that re-derives the
	-- same proposal across cycles appends its event rather than replacing the first, so "when did
	-- we first suspect this" is answerable. The decision side is single, because a proposal has
	-- exactly one terminal decision.
	proposed_by_event_ids text[] NOT NULL DEFAULT '{}'::text[],
	decided_by        text,
	decided_at        timestamptz,
	decision_event_id text,

	CONSTRAINT proposed_dependencies_status_check
		CHECK (status IN ('pending', 'confirmed', 'rejected', 'conflict')),
	-- A self-dependency carries no information and would make the triple ambiguous.
	CONSTRAINT proposed_dependencies_not_self CHECK (src_entity_id <> dst_entity_id),
	CONSTRAINT proposed_dependencies_key_matches
		CHECK (proposal_key = src_entity_id || '|' || dst_entity_id || '|' || edge_type),
	-- A decided row names who decided it and when; a pending row names neither. This is FR-041 in
	-- the schema rather than only in the projector: a decision with no authenticated individual
	-- behind it is what SC-011 counts as a failure, so the database refuses to store one.
	CONSTRAINT proposed_dependencies_decided_has_principal CHECK (
		(status IN ('pending', 'conflict') AND decided_by IS NULL AND decided_at IS NULL)
		OR (status IN ('confirmed', 'rejected') AND decided_by IS NOT NULL AND decided_at IS NOT NULL)
	)
);

-- What the review queue reads: the pending ones, strongest first.
CREATE INDEX IF NOT EXISTS proposed_dependencies_status_score_idx
	ON graph.proposed_dependencies (status, score DESC);

-- And "what has been proposed about this entity", from either end.
CREATE INDEX IF NOT EXISTS proposed_dependencies_src_idx
	ON graph.proposed_dependencies (src_entity_id);
CREATE INDEX IF NOT EXISTS proposed_dependencies_dst_idx
	ON graph.proposed_dependencies (dst_entity_id);

-- ---------------------------------------------------------------------------
-- The two decision bodies. Replaced rather than relaxed, as 0005 and 0009 did.
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
	'reject_dependency'
));
