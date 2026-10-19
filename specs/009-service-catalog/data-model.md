# Data Model: Service Catalog and Signal Attribution

Everything below is in the graph and its event log (constitution I). Nothing is stored outside it.
Wire shapes are in [contracts/catalog.md](./contracts/catalog.md).

## Catalog entry

A node of type `service`, ref `sre.catalog=<slug>` (R1).

| field | where | rule |
|---|---|---|
| slug | ref value | `[a-z0-9-]{1,63}`, minted once from the first display name, unique, never changed |
| display name | node display name | 1 to 120 characters; a rename is a new node version |
| kind | `sre.catalog.kind` | `service` \| `platform` |
| status | `sre.catalog.status` | `proposed` \| `confirmed` \| `hidden` |
| owner | `sre.catalog.owner` | optional free text, at most 120 characters |
| description | `sre.catalog.description` | optional, at most 1,000 characters, sanitised like 007 summaries |
| decided by | provenance | the `DeclareCatalogEntry` or `ProposeCatalogEntry` event; a principal for the former |

**Transitions**: `proposed → confirmed` (a person confirms), `proposed | confirmed → hidden` (a person
hides it: "not a service"), `hidden → confirmed` (restore). An entry is never deleted. Merging two entries
is 001's `ManualMerge`; splitting is 001's `SplitEntity`; both keep their history.

**Validation**: `DeclareCatalogEntry` without a principal is refused (001 FR-041). Any other event that
names the `sre.catalog` namespace from a source other than `sre.attributor` or a human decision is
refused with reason code `catalog_namespace_reserved`.

## Alias

A row of `graph.catalog_aliases`, bitemporal (R2).

| field | type | rule |
|---|---|---|
| `namespace`, `value` | string | a ref namespace and value, as signals carry them (R3) |
| `environment` | string, optional | matched only against signals that state one; empty matches every environment |
| `entry` | entity id | the entry the name denotes |
| `valid`, `observed` | intervals | constitution II |
| `decided_by` | event id | `DeclareAlias`, `CorrectAttribution` (name-level), `ProposeCatalogEntry` (an entity's own claims), or an import |
| `principal` or `rule_id` | string | one of the two, never neither (FR-050) |

**Uniqueness**: at one instant, a `(namespace, value, environment)` names at most one entry. Declaring
it for a second entry closes the first (a reassignment), and the audit shows both.

## Hidden name

A `(namespace, value)` an operator marked "not a service" (*Other*, *misc*). Stored as an alias whose
entry is the reserved `sre.catalog=-` (no node). A signal whose only names are hidden is `unknown` by
rule, and discovery never proposes the name again (FR-015).

## Attribution

An `ATTRIBUTED_TO` edge from a signal node to an entry (R8), from an `AttributeSignal` or
`CorrectAttribution` event.

| prop | rule |
|---|---|
| `sre.attribution.layer` | `correction` \| `alias` \| `probable` \| `classifier` |
| `sre.attribution.rule_id` | `H`, `A1`, `P1`–`P4`, `M` |
| `sre.attribution.ruleset` | e.g. `attribution/rules@1` |
| `sre.attribution.score` | 0 to 1; 1.0 for layers 0 and 1 |
| `sre.attribution.decision_event_id` | the event that made it |
| `sre.attribution.principal` | for corrections only |

An `AttributeSignal` that decides `unknown` creates no edge; it records the top three suggestions on the
event, so the audit query can answer "why is this unattributed?". The event also records each lower
layer's scores as evidence (FR-031).

## Correction

`CorrectAttribution`: a signal ref, the entries it is about (possibly none: "this is about no catalog
service"), `scope` (`signal` | `name`), the name ref when `scope = name`, a rationale and the principal.
A name-level correction also projects as an alias row decided by it. `RevertCorrection` names the
correction's event id; it closes what the correction opened and triggers re-attribution (R4).

## Proposals

- `ProposeCatalogEntry`: slug, display name, kind (always `service`), aliases from the entity's claims,
  the entity ref it came from. Projected as an entry with status `proposed`.
- `ProposeAlias`: a name ref, an entry, rule, score, rationale, evidence. Pending until `DeclareAlias`
  (accept) or `RejectAlias`. A rejected pair stays rejected; a later proposal of it is a conflict.

## Labelled attribution case (fixtures)

`fixtures/attribution/<case>/`: `events.jsonl` (recorded stream), `catalog.yaml`, optionally
`classifier.jsonl` (recorded classifier outputs by input digest), and `labels.yaml`:

```yaml
labels:
  - signal: datadog.monitor=12345/service:candidate-api   # the signal's ref
    expect: [candidate-view]                               # entry slugs, or [] for unknown
```
