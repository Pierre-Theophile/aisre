-- SPDX-License-Identifier: Apache-2.0
--
-- Deferred re-evaluation of a rule whose answer depends on the graph (004 T137,
-- specs/004-deploy-feeders/contracts/deploy-claims.md §2.1).
--
-- Resolution rules run in exactly one place: when a claim is stored. For every rule but C8 that is
-- enough, because their answers are functions of the claims they compare. C8 merges two change
-- observations that state the same deploy identifier FOR A TARGET THEY SHARE, and what makes two
-- targets the same target is a merge resolution already made — so its answer changes when the graph
-- changes under it, in two ways: a change attaches to a target that did not exist when its claim
-- arrived, or two target identities merge.
--
-- Re-evaluating in the transaction that caused the change does not work, and the reason is a property
-- of the temporal model rather than an implementation detail: a merge closes the observed interval of
-- the versions it absorbs, and `close_observed` refuses to close an interval at the instant it opened
-- — correctly, because a version that existed for no time is not a version. A second merge in one
-- transaction therefore tries to close what the first just opened and is refused.
--
-- So the work is recorded here and drained afterwards, one claim per transaction. That makes the
-- deferral a stated property rather than a hidden one: the merge lands on the next drain, which is
-- immediately after the provoking transaction commits.
--
-- This table is a WORK QUEUE and not history. Rows are deleted as they are processed, which is why it
-- is not one of the append-only relations scripts/check-migrations.sh protects: nothing here is
-- evidence, and everything it leads to is recorded in graph.resolution_decisions as usual.

CREATE TABLE IF NOT EXISTS graph.pending_resolution (
	-- The claim to re-evaluate. It is the primary key so that queuing the same work twice is one
	-- row: several merges in one transaction routinely touch the same change.
	claim_id  text PRIMARY KEY,
	-- Why it was queued, in words, so an operator reading the queue can tell a late attachment from
	-- a target merge without reconstructing it.
	reason    text NOT NULL,
	-- The event whose application queued it, so the work is traceable to a cause.
	event_id  text NOT NULL,
	queued_at timestamptz NOT NULL
);

COMMENT ON TABLE graph.pending_resolution IS
	'Claims whose resolution rules must be re-evaluated because the graph moved under them: a change '
	'attached to a new target, or two target identities merged. A work queue, not history — rows are '
	'deleted as they are drained, and every merge they cause is recorded in graph.resolution_decisions.';
