# Contract: Service Catalog and Signal Attribution

Published by ADR-0015 before code depends on it (R12). Everything is additive to schema 1.0.0.

## 1. Events (`graph.v1.EventEnvelope.body`)

```proto
ProposeCatalogEntry propose_catalog_entry = 30;
DeclareCatalogEntry declare_catalog_entry = 31;
DeclareAlias        declare_alias         = 32;
RetractAlias        retract_alias         = 33;
ProposeAlias        propose_alias         = 34;
RejectAlias         reject_alias          = 35;
AttributeSignal     attribute_signal      = 36;
CorrectAttribution  correct_attribution   = 37;
RevertCorrection    revert_correction     = 38;

message ProposeCatalogEntry { string slug = 1; string display_name = 2; Ref from_entity = 3; repeated Ref aliases = 4; }
message DeclareCatalogEntry {
  string slug = 1; string display_name = 2; string kind = 3;      // service | platform
  string status = 4;                                              // confirmed | hidden
  string owner = 5; string description = 6; string rationale = 7;
}
message DeclareAlias { Ref name = 1; string environment = 2; string entry_slug = 3; string rationale = 4; }
message RetractAlias { Ref name = 1; string environment = 2; string entry_slug = 3; string rationale = 4; }
message ProposeAlias {
  Ref name = 1; string environment = 2; string entry_slug = 3;
  string rule_id = 4; double score = 5; string rationale = 6; google.protobuf.Struct evidence = 7;
}
message RejectAlias { Ref name = 1; string environment = 2; string entry_slug = 3; string rationale = 4; }
message AttributeSignal {
  Ref signal = 1;
  repeated Attributed entries = 2;          // empty means unknown
  repeated Attributed suggestions = 3;      // top three below the threshold or margin
  string ruleset = 4;                       // attribution/rules@1
  google.protobuf.Struct evidence = 5;      // per-layer scores, classifier status
  string supersedes_event_id = 6;           // set on re-attribution
}
message Attributed { string entry_slug = 1; string layer = 2; string rule_id = 3; double score = 4; string rationale = 5; }
message CorrectAttribution {
  Ref signal = 1; repeated string entry_slugs = 2;
  string scope = 3;                         // signal | name
  Ref name = 4; string environment = 5; string rationale = 6;
}
message RevertCorrection { string correction_event_id = 1; string rationale = 2; }
```

**Principals**: `DeclareCatalogEntry`, `DeclareAlias`, `RetractAlias`, `RejectAlias`,
`CorrectAttribution` and `RevertCorrection` require an authenticated principal and are refused without
one (001 FR-041). `ProposeCatalogEntry`, `ProposeAlias` and `AttributeSignal` are accepted only from the
source `sre.attributor`.

**Idempotency keys**: proposals by `sha256(kind, slug or name, entry, rule)`; `AttributeSignal` by
`sha256(signal, ruleset, decision inputs digest)`; human decisions by `sha256(kind, body, principal,
decided_at)`, as 001's merges.

## 2. Edge type

`EdgeType.ATTRIBUTED_TO = 11` (signal → entry). Props as in [data-model.md](../data-model.md#attribution).

## 3. Query API (`graph.v1.CatalogService`)

| RPC | answers |
|---|---|
| `ListEntries(as_of, status?)` | entries with aliases and each alias's provenance (FR-005) |
| `GetEntry(slug, as_of)` | one entry, its aliases, and its decision history |
| `ListProposals(kind?, status?)` | proposed entries and alias suggestions with score and rationale (FR-010, FR-011) |
| `WhyAttributed(signal, as_of)` | the current decision, its layer, rule, score, rationale, evidence, the decisions it superseded, and corrections (FR-039) |
| `AttributedSignals(slug, as_of, limit, cursor)` | the signals attributed to an entry, and how (FR-039) |

All take 001's `AsOf` (valid and observed instants) and are authenticated like every query (001
FR-041a).

## 4. Command line

```text
aisre catalog list [--status proposed|confirmed|hidden] [--as-of ...]
aisre catalog show <slug>
aisre catalog proposals
aisre catalog confirm <slug> [--name "Candidate View"] [--kind service|platform]
aisre catalog declare <slug> --name ... [--kind ...] [--owner ...] [--description ...]
aisre catalog rename <slug> --name ...
aisre catalog hide <slug>|--name <ns>=<value>      aisre catalog restore ...
aisre catalog merge <slug-a> <slug-b>               aisre catalog split <slug> --detach <ns>=<value>...
aisre catalog alias add|remove|accept|reject <ns>=<value> <slug> [--env production]
aisre catalog import <catalog.yaml> | --from-notion-config <file> | --from-slack-flags <flags>
aisre attribute why <signal-ref>
aisre attribute correct <signal-ref> --entry <slug>... [--name <ns>=<value>]   # --name makes it name-level
aisre attribute revert <correction-event-id>
```

Every write command prints the event it appended and the principal it was signed as.

## 5. `catalog.yaml`

```yaml
version: 1
entries:
  - slug: candidate-view
    name: Candidate View
    kind: service
    owner: candidate team
    aliases:
      - datadog.log_service=candidate-api
      - notion.value=3fa18ebd99c281eca8fcee88402babed/Candidate View
      - { name: otel.service.name=candidate-api, environment: production }
  - slug: infra
    name: Infra
    kind: platform
hidden:
  - notion.value=3fa18ebd99c281eca8fcee88402babed/Other
```

## 6. Model configuration

```yaml
attribution:
  ruleset: attribution/rules@1
  threshold: 0.80
  margin: 0.10
  classifier:                 # optional; absent means layers 0–2 only
    kind: embedding           # uses the embedding block of 008
    timeout: 2s
    calibration: { a: 9.1, b: -6.4 }   # logistic, fitted on fixtures/attribution (T035)
```

## 7. `pkg/attribution`

```go
type Signal struct { Ref Ref; Kind string; Names []Name; Text string; ObservedAt time.Time }
type EntryProfile struct { Slug, DisplayName, Description string; Aliases []Name; DocumentTitles []string }
type Scored struct { Slug string; Score float64; Rationale string }

type Classifier interface {
    ID() string
    Classify(ctx context.Context, s Signal, entries []EntryProfile) ([]Scored, error)
}
```

`pkg/attribution/classifiertest` is the conformance suite every classifier runs: determinism on recorded
input, scores in [0, 1], no entry outside the profiles given, a context deadline honoured.
