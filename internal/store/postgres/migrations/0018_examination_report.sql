-- SPDX-License-Identifier: Apache-2.0
--
-- The examination report (002 FR-022b, ADR-0019 D1, T125).
--
-- Every concluded investigation says what it examined and what it did not: per hypothesis, the
-- signal families queried on its targets, the decisive families of its cause category that were
-- reachable and never queried (omissions), the ones no target could be asked for (coverage gaps),
-- and the entities in scope no call touched, with the expectation table and mode in force. It is
-- stored as the published message in its JSON form. A run concluded before this migration has
-- none, and nothing back-fills it: a concluded row is immutable (FR-007).

ALTER TABLE investigation.investigations
	ADD COLUMN IF NOT EXISTS examination jsonb;

COMMENT ON COLUMN investigation.investigations.examination IS
	'FR-022b: the examination report (sreagent.investigation.v1.ExaminationReport as JSON); NULL on runs concluded before 0018.';
