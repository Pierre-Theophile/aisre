-- SPDX-License-Identifier: Apache-2.0
--
-- 0005_actor_kind: the persistence half of the feature 001 change package (002 ADR-0005,
-- 002 tasks.md T019-T032). Two additive changes, both of them schema, neither of them history:
--
--   D3  a typed actor kind on the change projection;
--   D5  the five new event bodies the closed event vocabulary of 0001 has to admit.
--
-- Numbering: 0005 is the 001 change package and it is applied BEFORE 002's own schema, which is
-- 0006_investigation.sql. Migrations are numbered in the order they are applied, not in the
-- order they were written.
--
-- Nothing here drops, deletes or truncates: `ALTER TABLE ... DROP CONSTRAINT` replaces a CHECK
-- with a wider one, which adds accepted values and removes no row. scripts/check-migrations.sh
-- is the guard that says so.

-- ---------------------------------------------------------------------------
-- D3 -- actor kind on the change projection.
--
-- A change is stored as one canonical-JSON document in graph.entity_versions.change (0002), so
-- `actor_kind` already rides inside it the moment a feeder emits it: the column below is
-- GENERATED, not written, which is the point. A second, hand-maintained copy of a field could
-- disagree with the document it was copied from; a generated column cannot. It exists so that
-- "which changes did a controller make" is an index lookup rather than a scan, exactly as
-- entity_versions_change_kind_idx already does for `kind`.
--
-- NULL means the source said nothing -- the field is ACTOR_KIND_UNSPECIFIED, the enum's zero
-- value, which canonical serialisation omits entirely. That is genuinely distinct from the
-- string 'ACTOR_KIND_UNKNOWN', which means a feeder observed an actor and could not classify
-- it. The projector never guesses either one (ADR-0005 D1, 002 tasks.md T020).
ALTER TABLE graph.entity_versions
	ADD COLUMN IF NOT EXISTS change_actor_kind text
	GENERATED ALWAYS AS (change ->> 'actorKind') STORED;

COMMENT ON COLUMN graph.entity_versions.change_actor_kind IS
	'ActorKind enum name from the change document, or NULL when the source said nothing (ADR-0005 D1).';

CREATE INDEX IF NOT EXISTS entity_versions_change_actor_kind_idx
	ON graph.entity_versions (change_actor_kind)
	WHERE change_actor_kind IS NOT NULL;

-- ---------------------------------------------------------------------------
-- D5 -- the five new event bodies.
--
-- log.events.type is a closed vocabulary mirroring the `body` oneof of graph.proto, and 0001
-- says in as many words that adding a type is a migration plus a version bump. This is that
-- migration. The constraint is replaced rather than relaxed: an unknown type is still refused,
-- the set is simply five members larger.
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
	'alert_transition'
));
