# A connector over MCP

**Status: spike (T093).** It works, it is tested, and it is not a supported connector. What it
proves is the *shape* — that a feeder whose transport is the Model Context Protocol is an
ordinary feeder, with one client library instead of one vendor SDK per source. The code is
[`examples/mcp-feeder`](../../examples/mcp-feeder), a Go module of its own so that the
`sre-agent` binary does not link the MCP SDK.

```sh
cd examples/mcp-feeder
go test ./...                                   # conformance + a live run against a real MCP server
go run ./cmd/mcp-feeder --dry-run --polls 2 --interval 1ms \
    --environment prod --mcp-cmd "go run ./cmd/mcp-mockserver"
```

| | |
|---|---|
| MCP SDK | [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) v1.8.0, package `mcp` |
| Transports used | `CommandTransport` (stdio, spawns the server) and `StreamableClientTransport` (HTTP) |
| Client calls used | `Client.Connect`, `ClientSession.ListTools`, `ClientSession.CallTool`, `ClientSession.ReadResource`, `ClientSession.InitializeResult` |
| Fixture | [`examples/mcp-feeder/testdata/directory-01`](../../examples/mcp-feeder/testdata/directory-01) — 6 payloads, 23 events, two polls |

---

## 1. Why MCP is a connector transport at all

ADR-0001 D4 says a connector pulls from an external system "through its API or MCP surface".
The second half is not a hedge, and it is worth saying why.

**One client for many sources.** A Datadog connector, a Slack connector and a Notion connector
are three vendor SDKs, three auth dances, three pagination dialects and three sets of Go types
to keep up to date. Against MCP servers they are one client library and three allowlists. The
connector in `examples/mcp-feeder` is 957 lines of heavily commented Go; the MCP-specific part
is one file, `feeder/client.go`, and the rest is the mapping you would write anyway.

**Somebody else already wrote the read path.** Slack, Notion, GitHub, Sentry and Datadog all
ship or host MCP servers. A connector over one of those inherits its pagination, its retries and
its auth, and is a diff away from a connector over the next.

**Read-only is expressible.** MCP has a verb for "call a tool" and a verb for "read a
resource", and a server declares which tools it has. That makes the read-only rule (FR-046,
constitution VII) something a connector can state as a list of names in its own source code,
which a reviewer can check in a diff:

```go
// feeder/client.go
var allowedTools = []string{toolListChannels, toolListDocs}
var allowedResources = []string{resourceChanges}
```

`callTool` refuses anything not on that list before it touches the wire, and there is a test
that it does. The server's own `ReadOnlyHint` annotation is checked and *reported*, never
trusted — the Go SDK's own documentation says clients should not make tool-use decisions on
annotations from untrusted servers, and the annotation is a claim by the thing being policed.
The allowlist is the enforcement point; the hint is corroboration.

**The recorded fixture is the protocol's own answer.** A payload in
`testdata/directory-01/payloads/` is a marshalled `CallToolResult` or `ReadResourceResult`,
`_meta` and all — not a projection of one. That means the fixture stays meaningful when the
connector's decoding changes, and a reader can see exactly what the server said:

```json
{"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"acme-directory", ...}},
 "content":[{"type":"text","text":"{\"channels\":[{\"channel\":\"#team-checkout\", ...}]}"}],
 "structuredContent":{...},"resultType":"complete"}
```

---

## 2. What the spike reads

[`examples/mcp-feeder/mockserver`](../../examples/mcp-feeder/mockserver) is a real MCP server,
built with the same SDK, standing in for a vendor's. It exposes an ownership directory for a
fictional shop:

| MCP surface | Answers with |
|---|---|
| tool `list_channels` | `#team-checkout`, team slug `checkout`, `oncall: bob`, a channel URL, `owns: [cart, checkout]` — three channels |
| tool `list_docs` | four runbooks, each with a title, a URL and the services it `concerns` |
| resource `directory://changes` | announcements: "flag `checkout.new_pricing` enabled by bob at 08:41", with the services each one touched |

Its content is a pure function of a seed, and its `generatedAt` advances one tick per poll, so
two runs a month apart produce the same bytes — which is what lets the live test compare
against a recording. The third announcement happens *between* the two polls, so the fixture
exercises change detection rather than only the happy path.

---

## 3. The mapping

| MCP concept | Graph event | Notes |
|---|---|---|
| a channel from `list_channels` | `upsert_node` OWNER, ref `owner.team=#team-checkout` | props `sre.owner.kind=slack-channel`, `sre.owner.slack_channel`, `sre.owner.team`, `sre.owner.oncall`; a SOURCE_LINK pointer to the channel |
| the channel's `team` slug | `identity_claim` | subject `owner.team=#team-checkout`, claim `owner.team=checkout` — two names for the **same owner** |
| each name in the channel's `owns` | `upsert_edge` OWNED_BY, `otel.service.name=<svc>` → `owner.team=<channel>` | the service endpoint is usually a ref nothing has described yet; the graph mints a placeholder rather than refusing the edge |
| a runbook from `list_docs` | `upsert_node` SERVICE carrying a SOURCE_LINK pointer per runbook | `backend_kind: mcp:acme-directory`, `vocabulary: url`, selector = the document URL |
| an announcement from `directory://changes` | `observe_change` FLAG_FLIP, ref `mcp.change=<flag>@<id>` | actor and `valid_at` from the announcement, `origin_ref` the permalink, targets the service refs → `changed-by` edges |
| one poll (all three answers) | `source_checkpoint` | extent `[previous poll's answer, this poll's answer)`; the first declares `gap_before` |

Three decisions in that table are the interesting ones.

### A channel is not the service it owns

The obvious-looking move is an identity claim from `owner.team=#team-checkout` to
`otel.service.name=checkout`. It is wrong, and it is the kind of wrong that is invisible until
a blast-radius query is nonsense: a claim asks the resolver to consider merging two identities
into one entity, and an owner merged into the service it owns takes its on-call handle, its
channel URL and its ownership edges with it. Ownership is a relationship, so it is an edge.

The claim that *is* emitted relates two names of the owner itself — the channel `#team-checkout`
and the team slug `checkout` — which is exactly what a claim is for.

### A runbook is a pointer, not a node and not an edge

A runbook is not part of the production system; it is *where to look* when the thing it concerns
misbehaves, which is the definition of a pointer (constitution IV). A `depends_on` edge to a
document node would put a wiki page in the blast radius of an outage.

**The consequence, stated plainly, because it is a real cost.** Attaching a pointer means
emitting an `upsert_node` on a SERVICE ref, and this connector has almost nothing else to say
about that service. The event carries the pointers and one property, `service.name` (plus
`deployment.environment.name` when the operator supplied one). In the graph that version
*replaces* the `sre.placeholder=true` version an `owned_by` edge would otherwise have minted, so
the node stops being marked a placeholder while still being described by nobody who can see it
running. It is honest — the directory does assert that it knows of a service by that name — but
a reader of `query subgraph` sees a named service where before they saw an unnamed one, and
should not read that as topology. The alternative, hanging the runbook off the owner instead,
was rejected because the question a responder asks is "what do I read about *checkout*", not
"what does #team-checkout have on file".

### Almost nothing here knows when it became true

The directory says who owns what *now*. It does not say since when, and neither does a runbook.
So every owner node, every ownership edge and every documentation pointer is emitted with
`ValidFromUnknown: true`. Guessing the poll's timestamp would be a defect (FR-011) and would
also make the events depend on when the connector happened to run.

Announcements are the exception and the reason the resource exists: an announcement carries the
instant the change happened, so a `FLAG_FLIP` gets a real `valid_at` — 08:41, twenty minutes
before the connector ever started. That asymmetry is worth designing for in any directory-shaped
source: the standing facts are undated, the events are dated.

---

## 4. Limitations found

**No push, so polling — and the poll is the unit of everything.** MCP's
`notifications/tools/list_changed` says the *set of tools* changed, never that their answers
did. `resources/subscribe` exists but is optional, and a server that implements it still only
notifies for resources, not tool results. So a directory connector polls, and one poll is one
round of every call it makes. The connector groups a round by `Payload.Seq` and checkpoints it
when all three answers have arrived — never when the last payload happens to turn up, which
would make the extent depend on delivery order and fail `testkit.Shuffle`.

The first checkpoint of a run covers a zero-width extent and declares `gap_before: true`: on
its first poll the connector knows the directory as of one instant and nothing whatsoever about
before it. That is the correct statement, and it is also true of every restart.

**Change detection is "ask again and diff".** Standing facts get a content version from the
server (`"version":"6eca35c460657a43"`, a hash of the answer) and the connector puts it in the
event id: `mcp:acme-directory:owner:#team-checkout@6eca35c460657a43`. An unchanged directory
therefore mints the same ids and every re-poll is a `DUPLICATE_NOOP` — which is exactly what the
fixture shows, twenty-three events from two polls where the second poll contributed one
announcement and one checkpoint. A server that does *not* version its answers leaves the
connector hashing the response itself, which works but makes an id change whenever an unrelated
field does. **Ask an MCP server for a content version; it is the single most useful thing a
server can offer a connector.**

**Nothing dates its answers, either.** `Payload.At` has to come from the server, or a recording
is unreproducible and a shuffle check is meaningless. This connector requires a `generatedAt`
and errors out without one rather than falling back to `time.Now`. Real servers mostly do not
provide it — see §5.

**Pagination is per-server and mostly invisible.** The protocol has a cursor (`ListToolsParams.
Cursor`, and the same on resources) for *listing* tools and resources, but a tool's own result
is whatever JSON the server chose to return. A Slack server that caps `list_channels` at 100
expresses that in its own arguments, not in MCP's. A connector therefore cannot write a generic
paging loop: it pages the way each server's tools page, and the allowlist must include the
argument shape it will send. The spike sidesteps this — both its tools take no arguments — and
that is the largest thing it does not prove.

**Auth is entirely outside what this spike exercises.** Over stdio the credential is the child
process's environment. Over streamable HTTP the Go SDK offers
`StreamableClientTransport.OAuthHandler` and an `auth` package, and a hosted vendor server will
want an OAuth flow with token refresh — which is a per-vendor problem again, and the place
where "one client for many sources" stops being free. Worse for the read-only rule: an OAuth
scope is granted to the *session*, so a token that can call `list_channels` on a Slack MCP
server can very likely also call `send_message`. The allowlist bounds what the connector does;
it does not bound what the credential permits. Request a read-only scope where the vendor has
one, and say in the connector's README when it does not.

**`tools/list` can change under you.** The set of tools is not a stable API: a server may add,
remove or redefine one between two polls, and `notifications/tools/list_changed` announces
exactly that. The connector verifies at connect time that every tool it intends to call is
present and refuses to start otherwise (`Client.VerifyReadOnly`), but it does not re-verify per
poll. A long-running connector should, on the list-changed notification.

---

## 5. Slotting in a real Slack or Notion server

The mock's three surfaces were chosen to match what real servers actually expose.

**Slack.** `slack_search_channels` and `slack_list_channel_members` give the channel side;
`slack_read_channel` over a topology or ownership channel gives announcements. The mapping is
the one in §3 unchanged: channels become OWNER nodes, a channel's declared services become
`owned_by` edges. Two things change. First, Slack has no `owns` field — ownership has to come
from the channel topic, a pinned message or a convention like `#team-<service>`, and whichever
you pick is a heuristic that belongs in the connector with a comment saying so, not in the
graph. Second, a Slack message has a timestamp, so announcements read from a channel get a real
`valid_at` the way the spike's do; the standing facts still do not.

**Notion.** `notion-query-data-sources` over a "Services" database is the ownership directory,
and it is the better fit of the two: a Notion database has typed properties, so `owner`,
`oncall` and `services` are fields rather than a convention, and every page has a
`last_edited_time` that can serve as the content version. `notion-fetch` on a runbook page gives
the SOURCE_LINK pointer, and `notion-search` scopes the sweep. Pagination is real here — a
Notion database query returns a cursor — and is the case §4 says this spike does not cover.

**Datadog.** `search_datadog_entities` and `get_entity_tags` are an ownership directory in all
but name (service catalog teams, `#slack` tags), and `search_datadog_events` is a change feed
with real timestamps. This is the one where the MCP route competes directly with a plain API
connector, and the API probably wins: the vendor's API is versioned and the MCP surface is not.

In every case the read-only allowlist is three to five names, and the argument shapes are the
part that needs review — `slack_search_public` with the wrong query is a data-exfiltration
surface even though it is read-only.

---

## 6. Friction found in the SDK guide

Written as the guide's third real reader. Everything below is a suggestion for
[`writing-a-feeder.md`](./writing-a-feeder.md), not a defect in the SDK.

1. **The guide's worked example is a push-shaped source; every MCP connector is a pull-shaped
   one.** §8 shows a feeder draining payloads until EOF and checkpointing once at the end. A
   polling connector checkpoints per round, and has to decide what a round *is* in a way that
   survives `testkit.Shuffle`. That decision — group by `Payload.Seq`, emit when the round is
   complete, never on "the last payload I saw" — took longer than any other part of this spike
   and is the one thing a second MCP connector would copy verbatim. It deserves a section.

2. **`Payload.Seq` is documented as "the source-native sequence number (a Kubernetes
   resourceVersion, a Kafka offset)" and is silent on whether a puller may use it as a poll
   number.** It can, and it is the natural grouping key, but it then means something different
   from `Description.Ordering`: this connector declares `OrderingNone` — MCP answers carry no
   sequence the graph could order events by — while still numbering its payloads. Saying that
   the two are independent would have saved a detour.

3. **FR-046 is stated as "at the top of `Run`, ask the source system what your credential can
   do", but `Run` only receives a `Source` and an `Emitter`.** There is no handle on the client
   there. The pattern that worked is an optional interface the live `Source` implements and the
   recorded one does not:

   ```go
   if v, ok := src.(interface{ VerifyReadOnly(context.Context) error }); ok { … }
   ```

   so a replay skips the check rather than pretending to pass it. If that is the intended
   shape, the guide should show it — or the SDK should declare the interface, which would make
   it checkable.

4. **Nothing says what to do when a source cannot date its own answers.** The guide covers an
   unknown *valid* start (`ValidFromUnknown`) thoroughly, and says nothing about an unknown
   payload *arrival* time, which is what `Payload.At` is and what a shuffle permutes. The answer
   this connector settled on — refuse to start rather than fall back to `time.Now`, because the
   fallback silently makes the recording unreproducible — is worth stating.

5. **A pointer whose `backend_kind` names the server it came from must not be read from the
   session.** `ClientSession.InitializeResult().ServerInfo.Name` is right there and is the
   obvious source for `mcp:<server>`, but a replay has no session, so the pointer would differ
   between the live run and the fixture. It has to be configuration. The guide's pointer section
   could say the general rule: *nothing in an event may come from the connection, only from the
   payload or from configuration.*

6. **`sre.owner.oncall` does not exist.** `props.go` publishes `sre.owner.kind`,
   `sre.owner.team`, `sre.owner.slack_channel` and `sre.owner.source_label`, and the guide says
   to spell anything else yourself under `sre.`, which this connector does. If a second
   ownership connector wants it, it belongs in `pkg/feeder/props.go` so that the resolution
   rules can key on one spelling.

7. **Checked and found *not* to be a problem, because it looked like one:** `pkg/feeder` imports
   `internal/log`, and `record`/`testkit` import `internal/graph` and `internal/fixture`, which
   reads like the SDK cannot be used outside this repository. It can — Go's internal rule is
   applied per import edge, and the edge that matters (`pkg/feeder` → `internal/log`) is inside
   the tree. A module at `github.com/acme/anything` compiles against `pkg/feeder`,
   `pkg/feeder/record` and `pkg/feeder/testkit`. Worth a sentence in §11 so the next reader does
   not spend an hour on it.

One thing the guide gets exactly right and should keep: **"record with a batch size of 1"**, in
bold, with the story of the cluster run it cost. This spike records through
`emit.NewMemoryEmitter` rather than a graph, which answers per event, so
`record.ErrBatchingEmitter` never fired — but the warning is why the recording command was
written that way in the first place.

---

## 7. What this spike does and does not carry over

**Carries over:** the client code (~120 lines, transport-agnostic), the allowlist pattern and
its test, the "record the raw MCP result" decision, the poll-round checkpointing, the
`VerifyReadOnly`-on-the-Source pattern, and the whole mapping section — a real Slack directory
produces the same six event shapes.

**Does not carry over:** pagination (the spike's tools take no arguments), authentication (stdio
only), rate limiting and backoff, `tools/list_changed` handling, and the assumption that the
server dates and versions its answers — which is the spike's most load-bearing convenience and
the one real servers are least likely to grant.
