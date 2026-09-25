# Data Model: Temporal System Graph Core

**Feature**: 001-temporal-graph-core | **Date**: 2026-09-15 | Storage: PostgreSQL 16+

Two schemas: `log` (source of truth, append-only) and `graph` (projection, rebuildable from
`log`). All timestamps `timestamptz` in UTC. All intervals `tstzrange`, half-open `[)`.

## Schema `log`

### log.sources
| column | type | notes |
|---|---|---|
| source_id | text PK | e.g. `k8s:prod-eu1`, `otel:collector-a` |
| kind | text | `otel`, `k8s`, future connector kinds |
| ordering | text | `per_source_sequence` \| `none` |
| reordering_window | interval | declared by feeder |
| schema_version | text | event schema version the feeder emits |
| registered_at | timestamptz | |

### log.events (append-only)
| column | type | notes |
|---|---|---|
| event_id | text PK | source-supplied, globally unique |
| idempotency_key | text UNIQUE | defaults to event_id |
| source_id | text FK→sources | |
| source_seq | bigint NULL | per-source sequence if declared |
| schema_version | text | validated against accepted set |
| type | text | enum, see Event types |
| valid_at | timestamptz NULL | source-asserted valid time (start) |
| valid_end | timestamptz NULL | for retractions / bounded facts |
| valid_from_unknown | bool | FR-011 |
| source_observed_at | timestamptz NULL | informational (FR-019) |
| observed_at | timestamptz NOT NULL | assigned at append; read back on replay |
| principal | text NULL | authenticated individual for human decisions (FR-041) |
| trace_id | text NULL | self-telemetry correlation |
| payload | jsonb | typed body per `type` |
| appended_seq | bigserial | physical log order, used only for replay iteration |

Grants: app role INSERT+SELECT only. Indexes: `(source_id, source_seq)`, `(observed_at)`,
`(type, observed_at)`.

### log.rejected_events
| event_id_or_hash, source_id, received_at, reason_code, reason_detail, raw jsonb |

### log.duplicate_deliveries
| idempotency_key, received_at, payload_differs bool, raw_hash | (FR-020 audit)

### log.checkpoints
| source_id, checkpoint_at, extent_from, extent_to, gap_before bool, note | (FR-052, feeder gap)

## Schema `graph`

### graph.entities (canonical identity, not versioned)
| column | type | notes |
|---|---|---|
| entity_id | text PK | deterministic, research §4 |
| type | text | resolved node type (precedence rule, research §10); changes on merge |
| facets | text[] | all node types asserted by sources for this entity |
| created_by_event_id | text | provenance |
| merged_into | text NULL FK→entities | set when this id was merged away; queries follow redirect |

### graph.entity_versions (bitemporal)
| column | type | notes |
|---|---|---|
| version_id | text PK | deterministic |
| entity_id | text FK | |
| display_name | text | |
| valid | tstzrange NOT NULL | |
| observed | tstzrange NOT NULL | `upper_inf` = current knowledge |
| valid_from_unknown | bool | |
| valid_to_unknown | bool | |
| props | jsonb | OTel-conventional keys; each value stored as `{value, source_id, event_id}`; when two sources disagree, the key holds a list of such records and the version is flagged in `conflicts` |
| conflicts | text[] | prop keys with more than one asserted value (edge case "conflicting sources") |
| facets | text[] | asserted node types at this version (denormalized from entities for as-of reads) |
| pointers | jsonb | array of Pointer |
| change | jsonb NULL | only for type=change: kind, summary, actor, origin_ref |
| produced_by_event_ids | text[] | evidence (FR-034) |
| closed_by_event_id | text NULL | which event closed `observed` |

Invariants: for a given `entity_id` and any observed instant, the set of versions whose
`observed` contains it has pairwise non-overlapping `valid` ranges. Two sources asserting
different values for the same key at the same valid time therefore produce **one** version
whose `props[key]` lists both records and whose `conflicts` names the key; the projector never
picks a winner (exclusion constraint on
`(entity_id WITH =, valid WITH &&, observed WITH &&)`). Indexes: GiST `(valid, observed)`,
btree `(entity_id)`, GIN on `props` for name lookup, btree on `(change->>'kind')` partial.

### graph.edge_versions (bitemporal)
| column | type | notes |
|---|---|---|
| version_id | text PK | |
| src_id, dst_id | text FK→entities | |
| type | text | edge type enum |
| valid, observed | tstzrange | as above |
| valid_from_unknown, valid_to_unknown | bool | |
| weight_class | smallint NULL | `calls` only, 0–5 |
| props | jsonb | |
| produced_by_event_ids | text[] | |
| closed_by_event_id | text NULL | |
| closed_as_consequence_of | text NULL | version_id of a retracted node (edge case) |

Exclusion constraint on `(src_id, dst_id, type WITH =, valid WITH &&, observed WITH &&)`.
Indexes: GiST `(valid, observed)`, btree `(src_id)`, `(dst_id)`, `(type)`.

### graph.identity_claims
| claim_id PK (deterministic), entity_id FK, namespace, value, attributes jsonb, source_id,
observed_at, event_id | UNIQUE `(namespace, value, source_id)`. Index `(namespace, value)`.

### graph.resolution_decisions (append-only history)
| decision_id PK, kind (`auto_merge`,`suggest`,`confirm`,`reject`,`split`,`manual_merge`),
surviving_id, merged_id, rule_id NULL, score numeric NULL, rationale text,
supporting_claim_ids text[], principal text NULL, decided_at, event_id, superseded_by NULL |

### graph.suggestions (current view)
| pair_key PK (`min(id)|max(id)`), entity_a, entity_b, rule_id, score, rationale,
created_at, status (`pending`,`confirmed`,`rejected`,`conflict`), decision_id NULL |

### graph.principals
| principal PK (`iss|sub`), email, display_name, first_seen, last_seen, roles text[] |

## Event types (payload shapes; canonical in contracts/graph.proto)

| type | payload | effect |
|---|---|---|
| `upsert_node` | entity ref (type + primary claim), display_name, props, pointers, valid_at, valid_from_unknown | new entity if unseen; new version if any field differs; splits/closes overlapping valid ranges |
| `upsert_edge` | src ref, dst ref, edge type, props, weight_class, valid_at | as above for edges |
| `retract_node` | entity ref, valid_end | closes valid range of current version; cascades to edges with `closed_as_consequence_of` |
| `retract_edge` | src, dst, type, valid_end | closes valid range |
| `observe_change` | kind, summary, actor, origin_ref, targets[] (refs), valid_at, valid_end NULL | creates change entity + `changed-by` edges; unattached if target unresolved; later attach on target appearance |
| `identity_claim` | entity ref, namespace, value, attributes | stores claim; triggers resolution pass |
| `confirm_merge` | entity_a, entity_b, rationale | human; merge with precedence |
| `reject_merge` | entity_a, entity_b, rationale | human; blocks pair |
| `split_entity` | entity_id, claims_to_detach[], rationale | human; new entity from detached claims |
| `manual_merge` | entity_a, entity_b, rationale | human |
| `source_checkpoint` | extent_from, extent_to, gap_before | records extent/gaps |

Refs use `{namespace, value}` so feeders never need canonical ids.

## State transitions

**Entity version lifecycle**: `current (observed upper_inf)` → `superseded` (observed closed by
correction) or → `retracted` (new version with bounded valid, previous superseded). No
deletes.

**Suggestion**: `pending` → `confirmed` | `rejected` (human) ; `pending`/`confirmed` →
`conflict` when a new claim contradicts a human decision (surfaced, never applied).

**Entity merge**: `A, B distinct` → (certain rule or human) → `B.merged_into = A`, claims of B
re-pointed to A, versions of B kept (history) with a redirect; `split` reverses by creating
`C` from detached claims, never resurrecting `B`'s id.

**Change attachment**: `unattached` → `attached` when a target ref resolves; the attach is
recorded as an edge version with `produced_by_event_ids` = [change event, node event].

## Validation rules (FR-009, FR-024)

Rejected with `reason_code`: `untyped`, `unknown_schema_version`, `missing_valid_time`,
`telemetry_payload` (denylist: keys `metric_value`, `value`, `log_body`, `body`, `span_id`,
`trace_id` inside props; arrays of numeric samples; any prop > 4 KiB), `secret_value`
(config node of kind secret with a `data`/`value` prop), `unknown_type`, `ref_unresolvable`
(only for retractions; upserts create).

## Derived, rebuildable structures

None persisted in v1. As-of adjacency slices are computed per query. If the benchmark shows
need, a `graph.adjacency_cache` may be added; it must be droppable and rebuilt from
`*_versions` (Principle I).
