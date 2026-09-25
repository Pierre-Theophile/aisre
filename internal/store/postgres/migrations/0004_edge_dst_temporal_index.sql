-- SPDX-License-Identifier: Apache-2.0
--
-- 0004_edge_dst_temporal_index: the missing half of the as-of adjacency index (research §2,
-- SC-001, SC-002, ADR-0002).
--
-- Every read in the graph starts the same way: take a frontier of entity ids and load the edges
-- incident to them as of an instant. The query the engine issues per hop is
--
--   SELECT ... FROM graph.edge_versions
--   WHERE valid @> $t AND (upper_inf(observed) OR observed @> $o)
--     AND (src_id = ANY($frontier) OR dst_id = ANY($frontier))
--
-- and that OR is served as a BitmapOr of two index scans, one per side. Until this migration
-- only the *outgoing* side had an index that could answer it: the exclusion constraint
-- `edge_versions_no_overlap` is a GiST index on (src_id, dst_id, type, valid, observed), so
-- `src_id = ANY(...) AND valid @> $t` is a cheap prefix search. The incoming side had only the
-- plain btree `edge_versions_dst_id_idx`, which cannot answer a range containment at all, so
-- the planner used the exclusion index with `dst_id` as a non-leading column — which means
-- scanning essentially the whole index.
--
-- Measured on the reference benchmark's 1k workload (37k edge versions, 400-id frontier,
-- `fixture bench --scale 1k --keep-db`, EXPLAIN ANALYZE):
--
--   before   232 ms   BitmapOr: src side 5 ms / 1 428 buffers, dst side 218 ms / 121 204 buffers
--   after      8 ms   BitmapOr: src side 3 ms / 1 496 buffers, dst side  3 ms /   1 620 buffers
--
-- The dst side was 96% of the query and 99% of the buffers read, and this is the query every
-- subgraph hop, every diff slice and every impact expansion runs, so it was the whole budget.
--
-- The fix is the index research §2 describes and the schema was missing: the same GiST shape as
-- the exclusion constraint, led by `dst_id`. It is additive — a new index, no column, no
-- rewrite, no data touched — so it changes no query's answer and no fixture's golden output,
-- only how long the answer takes. `edge_versions_dst_id_idx` is kept: a plain equality lookup
-- with no temporal predicate is still better served by the btree.
--
-- btree_gist is already installed by 0002_graph.sql, which the exclusion constraints need.

CREATE INDEX IF NOT EXISTS edge_versions_dst_temporal_idx
	ON graph.edge_versions USING gist (dst_id, valid, observed);

COMMENT ON INDEX graph.edge_versions_dst_temporal_idx IS
	'Incoming half of the as-of adjacency slice: dst_id = ANY(frontier) AND valid @> t (research section 2).';
