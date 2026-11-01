-- SPDX-License-Identifier: Apache-2.0
--
-- The search-space profile (002 FR-012a, ADR-0019 D1, T123).
--
-- The investigator's first brief carries a profile of the scope: the entities near the subject,
-- the edges between them, which signal families can be queried about each, and the blind spots.
-- The investigation records the profile it was shown, so that the examination report (FR-022b)
-- and a reader can see what was reachable. It is rebuilt from the neighbourhood read on replay;
-- the column is the copy a reader does not need a replay to see. A run concluded before this
-- migration has none, and nothing back-fills it: a concluded row is immutable (FR-007).

ALTER TABLE investigation.investigations
	ADD COLUMN IF NOT EXISTS search_space_profile text;

COMMENT ON COLUMN investigation.investigations.search_space_profile IS
	'FR-012a: the rendered search-space profile the first brief carried; NULL on runs concluded before 0017 or that never read the neighbourhood.';
