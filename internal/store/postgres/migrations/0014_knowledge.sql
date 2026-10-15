-- SPDX-License-Identifier: Apache-2.0
--
-- 0014_knowledge: the default knowledge store (feature 008, research R2, ADR-0014 item 3).
--
-- A document's content lives here, outside the graph (constitution 1.2.0: the graph scopes and
-- dates the RAG index and carries no document content). Each section version is kept with the
-- interval it was known over, so a replay matches exactly the text the original run could read.
--
-- Nothing beyond PostgreSQL itself: the lexical index is a tsvector on the `simple` configuration
-- (no stemming, so French and English postmortems score alike), and vectors are plain real[],
-- scanned exactly in process. No pgvector: many managed and self-hosted installs lack it, and a
-- deployment that outgrows the exact scan plugs in another store behind pkg/knowledge.Store.
--
-- section_versions is history, as the graph's version tables are: rows are inserted, and the one
-- permitted mutation is closing an open `known` interval when a put changes or drops the section.

BEGIN;

CREATE SCHEMA IF NOT EXISTS knowledge;

CREATE TABLE IF NOT EXISTS knowledge.section_versions (
	version_id   bigserial PRIMARY KEY,
	document_ref text NOT NULL,
	label        text NOT NULL,
	part         integer NOT NULL,
	digest       text NOT NULL,
	origin       text NOT NULL,
	text         text NOT NULL,
	known        tstzrange NOT NULL,
	search       tsvector GENERATED ALWAYS AS (to_tsvector('simple', label || ' ' || text)) STORED,
	CONSTRAINT section_versions_part_nonnegative CHECK (part >= 0),
	CONSTRAINT section_versions_label_nonempty CHECK (length(btrim(label)) > 0),
	CONSTRAINT section_versions_text_bounded CHECK (char_length(text) BETWEEN 1 AND 1200),
	CONSTRAINT section_versions_origin_published
		CHECK (origin IN ('property', 'body', 'summary', 'verdict')),
	CONSTRAINT section_versions_known_nonempty CHECK (NOT isempty(known)),
	-- One section never has two versions known at the same instant.
	CONSTRAINT section_versions_no_overlap EXCLUDE USING gist (
		document_ref WITH =,
		label WITH =,
		part WITH =,
		known WITH &&
	)
);

COMMENT ON COLUMN knowledge.section_versions.known IS
	'Known-time interval, half-open [). upper_inf() means the current version.';

-- At most one open version per section, and the read a put starts with.
CREATE UNIQUE INDEX IF NOT EXISTS section_versions_open_idx
	ON knowledge.section_versions (document_ref, label, part)
	WHERE upper_inf(known);

CREATE INDEX IF NOT EXISTS section_versions_search_idx
	ON knowledge.section_versions USING gin (search);

CREATE INDEX IF NOT EXISTS section_versions_document_idx
	ON knowledge.section_versions (document_ref);

-- Vectors, per section digest and model: identical text in two documents is embedded once.
CREATE TABLE IF NOT EXISTS knowledge.section_embeddings (
	digest      text NOT NULL,
	model       text NOT NULL,
	dimensions  integer NOT NULL,
	vector      real[] NOT NULL,
	computed_at timestamptz NOT NULL DEFAULT now(),
	seq         bigserial NOT NULL,
	PRIMARY KEY (digest, model),
	CONSTRAINT section_embeddings_dimensions
		CHECK (dimensions > 0 AND cardinality(vector) = dimensions)
);

-- The in-process cache refreshes incrementally by seq.
CREATE INDEX IF NOT EXISTS section_embeddings_model_seq_idx
	ON knowledge.section_embeddings (model, seq);

COMMIT;
