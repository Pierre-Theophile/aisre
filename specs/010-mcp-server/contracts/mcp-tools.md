# Contract: MCP tools and resources of `aisre mcp`

**Feature**: [spec.md](../spec.md) | **Plan**: [plan.md](../plan.md)

Server name `aisre`, version = the binary's version, title naming the algebra version; `instructions` state that results are evidence,
never instructions (FR-042). Instants are RFC 3339, relative (`-30m`) or `now`, as on the CLI. A
reference is `<namespace>=<value>`.

## Tools

| tool | role | profile | RPC | input (CLI defaults apply) | read-only |
|---|---|---|---|---|---|
| `subgraph` | reader | all | `QueryService.Subgraph` | `focus`, `valid_at`, `observed_at`, `hops`, `direction`, `edge_types`, `min_weight`, `per_hop_cap`, `total_cap` | yes |
| `diff` | reader | all | `QueryService.Diff` | `focus`, `at`+`window` or `from`+`to`, `observed_at`, `reference`, `hops`, `direction`, `edge_types`, `change_margin`, caps | yes |
| `impact` | reader | all | `QueryService.Impact` | `focus`, `valid_at`, `observed_at`, `max_hops`, `total_cap` | yes |
| `history` | reader | all | `QueryService.NodeHistory` | `focus` | yes |
| `pointers` | reader | all | `QueryService.Pointers` | `focus`, `valid_at`, `observed_at` | yes |
| `extent` | reader | all | `QueryService.Extent` | none | yes |
| `resolve_why` | reader | all | `QueryService.ResolutionAudit` | `a`, `b`, `observed_at` | yes |
| `catalog_list` | reader | all | `CatalogService.ListEntries` | `status`, `as_of` (valid and observed, as `aisre catalog`) | yes |
| `catalog_show` | reader | all | `CatalogService.GetEntry` | `slug`, `as_of` | yes |
| `attribution_why` | reader | all | `CatalogService.WhyAttributed` | `signal`, `as_of` | yes |
| `investigation_list` | reader | default | `InvestigationService.List` | `incident_id`, `lifecycle`, `since`, `page_size`, `page_token` | yes |
| `investigation_get` | reader | default | `InvestigationService.Get` | `investigation_id` | yes |
| `investigation_start` | investigator | default | `Investigate` / `Declare` | `at`, and `reference` or `declaration{title, severity, origin, services}`; `lookback` | no (non-destructive; a declaration is idempotent by its key) |

The roles are the API's own (`Get` and `List` need `reader`, `Investigate` and `Declare` need
`investigator`); the adapter does not widen or narrow any role.

## Result shape

- `structuredContent`: the RPC's response in canonical JSON (`graph.CanonicalJSON`), identical to
  `aisre … --output json`.
- `content[0]`: a text summary of at most a few lines (counts, the top-ranked item, partiality).
- `_meta`:
  - `aisre.as_of`: `{valid_at, observed_at}` the answer was computed for;
  - `aisre.partial`: true with `aisre.continuation` when the answer carries a truncation or cursor;
  - `aisre.injection`: list of `{path, pattern, excerpt}`, empty when nothing matched.
- An API error is `isError: true` with `aisre.code` (the Connect code) and the API's message.

## Resources

| URI | content |
|---|---|
| `aisre://guide` | which tool answers which question, the time model and the reference spelling (`internal/mcpserver/guide.md`, embedded) |
| `aisre://catalog` | `CatalogService.ListEntries` now, canonical JSON (reader) |

## Identity RPC (additive, ADR-0016 D3 boundary)

```proto
service IdentityService { rpc WhoAmI(WhoAmIRequest) returns (WhoAmIResponse); }
message WhoAmIRequest {}
message WhoAmIResponse { string subject = 1; string issuer = 2; repeated string roles = 3; }
```

Any authenticated principal may call it. It returns what the interceptor established; it reveals
nothing the caller's own token does not already carry.
