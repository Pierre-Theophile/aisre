-- SPDX-License-Identifier: Apache-2.0
--
-- Propagation paths (002 FR-019b, ADR-0019 D1, T128).
--
-- Every supported hypothesis, and the top-ranked one, carries the chain of typed graph edges from
-- its cause to the alerting entity, each edge citing the evidence that shows the fault crossed it
-- or marked unverified, and an undrawable path names its missing link. It is stored as the
-- published message in its JSON form. A run concluded before this migration has none, and nothing
-- back-fills it: a concluded row is immutable (FR-007).

ALTER TABLE investigation.investigations
	ADD COLUMN IF NOT EXISTS propagation_paths jsonb;

COMMENT ON COLUMN investigation.investigations.propagation_paths IS
	'FR-019b: the propagation paths (sreagent.investigation.v1.PropagationPaths as JSON); NULL on runs concluded before 0019.';
