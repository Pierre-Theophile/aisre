-- SPDX-License-Identifier: Apache-2.0
--
-- 0008_evidence_source_of_truth: the two things 0006 left to the code (002 FR-007, FR-022,
-- data-model.md §investigation.evidence_items, §investigation.investigations "Immutability").
--
-- Two changes, both of them turning a rule that was enforced in Go into a rule the database
-- enforces. Each is here because "enforced by convention" is how an invariant stops being one.
--
-- 1. `evidence_items.source_of_truth`.
--
-- FR-022 is that a hypothesis may only be marked `supported` on evidence from a worker that
-- speaks for a source of truth. The ledger enforces that at write time, with the item in memory,
-- and 0006 had no column for the declaration — so `internal/investigation/store/ledger_dao.go`
-- carried a note saying a reader had to re-derive it from the worker registry. Re-derivation is
-- only sound while the registry still looks the way it looked during the run: a worker retired,
-- renamed or re-pointed at a different backend makes a historical answer unreadable in exactly
-- the case the audit trail exists for. The column records what the worker declared *at the time*,
-- which is the stronger form of FR-022 and the one worth having.
--
-- It is added NOT NULL DEFAULT '' rather than nullable. An empty declaration is a real and
-- meaningful value — a knowledge item speaks for no source of truth, and that is why it cannot
-- carry a hypothesis to `supported` — so the distinction between "declared nothing" and "we never
-- recorded it" is not one the column needs, and existing rows read as the former, which is what
-- they were treated as anyway.
--
-- 2. The concluded-row immutability trigger.
--
-- FR-007: "a completed investigation is immutable; a later run is a new row linked by
-- reopens_investigation_id, never an edit of this one." Until now that was a guarded UPDATE in
-- one DAO — every statement carrying `AND lifecycle = 'running'` in its WHERE clause — which is
-- exactly as strong as every future author remembering to write it. data-model.md already
-- specifies the trigger; this is it.
--
-- The single permitted transition is `concluded → reopened` and nothing else on the row may move
-- with it (MarkReopenedInTx). A `reopened` row records only that a child exists; the verdict, the
-- ledger and the evidence of the concluded run stay exactly as produced, which is what "the
-- concluded record MUST remain readable exactly as produced" means.
--
-- Scope, stated so the next author does not have to guess: the trigger guards rows whose OLD
-- lifecycle is `concluded`. `failed` and `reopened` rows are left alone deliberately — nothing
-- writes to them today, and widening the guard to them in the same migration would couple an
-- FR-007 fix to a rule nobody has needed yet. The ordering consequence for callers is real and
-- worth knowing: anything that links a conclusion to what produced it — the decision record's
-- `decision_event_id`, `recording_key`, `recording_digest` — must be written *before* the row is
-- concluded, because afterwards the row is closed. `internal/investigation/runner` does exactly
-- that, and its comment says why.
--
-- Nothing here drops, deletes or truncates: one ADD COLUMN, one function, one trigger.
-- scripts/check-migrations.sh is the guard that says so.

BEGIN;

-- ---------------------------------------------------------------------------------------
-- 1. The source-of-truth declaration on every evidence item
-- ---------------------------------------------------------------------------------------

ALTER TABLE investigation.evidence_items
	ADD COLUMN IF NOT EXISTS source_of_truth text NOT NULL DEFAULT '';

COMMENT ON COLUMN investigation.evidence_items.source_of_truth IS
	'The single source of truth the answering worker declared, as of the call (FR-022). Empty means the worker speaks for none -- a knowledge item, a human fact -- which is why such an item cannot carry a hypothesis to `supported`.';

-- Reading "which evidence came from a source of truth" is the FR-022 question, asked per
-- investigation, so the index carries the investigation with it.
CREATE INDEX IF NOT EXISTS evidence_items_source_of_truth_idx
	ON investigation.evidence_items (investigation_id, source_of_truth);

-- ---------------------------------------------------------------------------------------
-- 2. A concluded investigation is immutable
-- ---------------------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION investigation.refuse_update_of_concluded()
	RETURNS trigger
	LANGUAGE plpgsql
	AS $refuse_update_of_concluded$
BEGIN
	-- Only a concluded row is closed. A running row is a run in flight and is written to
	-- constantly; a failed or reopened one is out of scope here (see the header).
	IF OLD.lifecycle IS DISTINCT FROM 'concluded' THEN
		RETURN NEW;
	END IF;

	-- The one permitted transition (FR-007, FR-057b): the lifecycle moves to `reopened` and
	-- every other column is untouched. Comparing the rows as jsonb minus that one key is exact
	-- and needs no per-column list, so a column added by a later migration is covered by this
	-- rule from the moment it exists rather than from the moment somebody remembers it.
	IF NEW.lifecycle = 'reopened'
		AND (to_jsonb(NEW) - 'lifecycle') = (to_jsonb(OLD) - 'lifecycle') THEN
		RETURN NEW;
	END IF;

	RAISE EXCEPTION
		'investigation % concluded at % and is immutable; a later run is a new row linked by reopens_investigation_id, and the only permitted change to this one is lifecycle = ''reopened'' (002 FR-007)',
		OLD.investigation_id, OLD.ended_at
		USING ERRCODE = 'check_violation';
END;
$refuse_update_of_concluded$;

COMMENT ON FUNCTION investigation.refuse_update_of_concluded() IS
	'FR-007: a concluded investigation is never edited. The single permitted transition is lifecycle = ''reopened'', which records that a child exists and changes nothing else.';

DROP TRIGGER IF EXISTS investigations_concluded_is_immutable ON investigation.investigations;
CREATE TRIGGER investigations_concluded_is_immutable
	BEFORE UPDATE ON investigation.investigations
	FOR EACH ROW EXECUTE FUNCTION investigation.refuse_update_of_concluded();

COMMIT;
