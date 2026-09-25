-- SPDX-License-Identifier: Apache-2.0
--
-- 0003_facets: every node type a source has asserted for an entity (ADR-0001 D7, research §10).
--
-- A Kubernetes Deployment and the OpenTelemetry service running on it are one entity under two
-- names. When certain rule C2 or C3 merges them, the survivor is asserted as WORKLOAD by one
-- source and as SERVICE by the other. Neither assertion may be thrown away: `type` keeps the
-- resolved type chosen by the published precedence
--
--   SERVICE > WORKLOAD > THIRD_PARTY > INFRA_RESOURCE > DB_SCHEMA > CONFIG > FEATURE_FLAG >
--   OWNER > ALERT > CHANGE
--
-- and `facets` keeps the whole set, so nothing a source said is lost.
--
-- The column is added to graph.entities (current resolved identity) and to
-- graph.entity_versions (what was asserted as of that version, so an as-of read does not have
-- to consult the current entity row). A facet change is therefore a new version, closed and
-- reopened like any other correction (constitution II) -- which is why the version table needs
-- the column at all.
--
-- Both are stored ordered by the precedence above, highest first, so the array is a canonical
-- value and two runs of the projector produce the same bytes (FR-023).
--
-- Additive only: no data is dropped, no relation is rewritten. Existing rows get the empty
-- array, which reads as "no source has asserted a type for this entity yet" -- the state of a
-- placeholder entity created to satisfy an edge endpoint before its own upsert_node arrives.

BEGIN;

ALTER TABLE graph.entities
	ADD COLUMN IF NOT EXISTS facets text[] NOT NULL DEFAULT '{}'::text[];

ALTER TABLE graph.entity_versions
	ADD COLUMN IF NOT EXISTS facets text[] NOT NULL DEFAULT '{}'::text[];

COMMENT ON COLUMN graph.entities.facets IS
	'Every node type asserted for this entity by any source, precedence-ordered; `type` is the first of them (research §10, ADR-0001 D7).';
COMMENT ON COLUMN graph.entity_versions.facets IS
	'Node types asserted as of this version, precedence-ordered. A change of facets is a new version.';

-- Entities are looked up by facet ("every entity any source calls a SERVICE"), which a GIN
-- index on the array answers without scanning.
CREATE INDEX IF NOT EXISTS entities_facets_idx
	ON graph.entities USING gin (facets);

COMMIT;
