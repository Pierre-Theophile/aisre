# Writing a feeder

A **feeder** is a connector: it turns one external system — Kubernetes, OpenTelemetry, a cloud
provider, a code host, a chat tool — into typed events in the temporal graph. This page is
everything you need to write one, in the order you will need it. It assumes you have never
opened this repository and does not ask you to: the whole public surface is one Go package,
[`pkg/feeder`](../../pkg/feeder), and everything below is written against it.

Budget about a day, from an empty file to a passing conformance test (SC-008). Walked end to
end on 2026-09-17 against this page alone — a made-up feature-flag source, five recorded
payloads, a hand-authored fixture, `testkit.Conformance` green — the mechanical work took about
twenty minutes; the rest of a day is reading, and understanding your own source system.

### Getting the SDK

The module is **not published to a proxy yet** and carries no version tag, so `go get
github.com/Pierre-Theophile/aisre` does not resolve: it stalls against `proxy.golang.org` and
then fails. Until the first tagged release, work from a clone and point your module at it.

```sh
git clone https://github.com/Pierre-Theophile/aisre
mkdir my-feeder && cd my-feeder
go mod init example.com/my-feeder

# Either a replace directive in go.mod …
go mod edit -require=github.com/Pierre-Theophile/aisre@v0.0.0 \
            -replace=github.com/Pierre-Theophile/aisre=../sre-agent
# … or a workspace, which also lets `go doc` resolve the packages below.
go work init . ../sre-agent
```

Go needs Go 1.25 or later and `CGO_ENABLED=0` is fine: the SDK has no C dependency. Once that
is wired, `go doc github.com/Pierre-Theophile/aisre/pkg/feeder` is the reference this page is a
guided tour of, and every symbol named here is in it.

---

## 1. The idea in one paragraph

The graph is a **bitemporal, event-sourced** record of what your production system looked like
and when you learned it. A feeder never writes to the graph directly and never answers
questions about its source system; it emits typed, idempotent events, and everything else —
versioning, merging, querying, ranking — is the graph's job. Two consequences shape every
feeder ever written:

- **The graph stores where to look, never what was measured.** No metric sample, log line or
  span ever goes into a property. You attach a *pointer* — a selector in OpenTelemetry semantic
  conventions that says how to fetch that telemetry from the backend that owns it. An event
  that carries telemetry is refused, by the SDK before it leaves your process and by the server
  if you get past that.
- **Every event is replayable.** The same payloads must produce the same events, byte for byte,
  on every run and in every process. That means deterministic identifiers derived from
  source-native ones — an object UID plus its resourceVersion, an aggregation window key —
  never a UUID, a counter or a clock reading.

---

## 2. The interface

Three types, and you implement one of them.

```go
// Feeder turns one external system into typed graph events.
type Feeder interface {
    Describe() Description
    Run(ctx context.Context, src Source, em Emitter) error
}

// Source abstracts live versus recorded input, so both share one code path.
type Source interface {
    Next(ctx context.Context) (Payload, error) // io.EOF when exhausted
}

// Emitter validates and forwards events. Validation errors are returned, never swallowed.
type Emitter interface {
    Emit(ctx context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error)
    Checkpoint(ctx context.Context, from, to time.Time, gapBefore bool) error
    Flush(ctx context.Context) error
}
```

`Source` and `Emitter` are the seam that makes a feeder testable. You never construct either:
running live, the harness hands you a `source.ChanSource` your watch loop pushes into and an
`emit.ConnectEmitter` pointed at a real graph; running from a recording, it hands you a
`source.FileSource` and an in-memory emitter. Your `Run` cannot tell the difference, which is
what makes "the recorded mode is the test" true rather than aspirational (FR-044).

### `Describe`

```go
func (f *myFeeder) Describe() feeder.Description {
    return feeder.Description{
        SourceID:         "myvendor:prod-eu1",  // your identity; a token is scoped to exactly one
        Kind:             "myvendor",           // the connector family
        Ordering:         feeder.OrderingPerSourceSequence,
        ReorderingWindow: 60 * time.Second,
        RequiredScopes:   []string{"read:topology"},
        Namespaces:       []string{feeder.NSOTelService, feeder.NSServerAddress},
    }
}
```

Every field is a promise the test harness holds you to.

| Field | What it promises |
|---|---|
| `SourceID` | Prefixes every event id you mint. Your credential is scoped to it; you cannot write for anyone else. |
| `Kind` | The connector family. Fixtures group payload directories by it. |
| `Ordering` | `OrderingPerSourceSequence` if you number your events (a resourceVersion, a log offset), `OrderingNone` otherwise. |
| `ReorderingWindow` | How far out of order your source may deliver. `testkit.Shuffle` permutes payloads inside it and demands you emit the same facts. Declaring a **wider** window than the truth makes the test stricter, never weaker. |
| `RequiredScopes` | Documentation of the read-only permissions you ask for (§7). |
| `Namespaces` | **Every** namespace that appears in **any** ref you emit — not only the ones you mint. An edge's `Dst`, a claim's `Claim` and a change's `Targets` usually point at another connector's namespaces, and those count too. The testkit fails a run that emits a ref outside this set, naming the namespace and the first event that used it, which is what stops two connectors quietly claiming each other's identities. |

`SchemaVersion` defaults to `feeder.SchemaVersion` (`"1.0.0"`) and you normally leave it alone.

---

## 3. Emitting events

There is one constructor per event type. Each takes your `Description`, the event id, and a
struct describing the fact; the description fills in the source id, the schema version and the
idempotency key.

```go
ev := feeder.UpsertNode(d, feeder.NewID(d.SourceID, "workload", "shop/checkout@rv1001"),
    feeder.NodeFact{
        Meta:        feeder.Meta{Seq: 1001},
        Ref:         feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
        Type:        graphv1.NodeType_WORKLOAD,
        DisplayName: "checkout",
        Props:       props,      // from a feeder.Props builder, see below
        Pointers:    pointers,   // see §5
        ValidAt:     seenAt,     // when the fact became true, as your source asserts it
    })
result, err := em.Emit(ctx, ev)
```

| Constructor | Fact struct | Use it for |
|---|---|---|
| `UpsertNode` | `NodeFact` | an entity exists and looks like this from `ValidAt` onwards |
| `UpsertEdge` | `EdgeFact` | a relationship holds from `ValidAt` onwards |
| `RetractNode` | `NodeRetraction` | an entity stopped existing at `ValidEnd` |
| `RetractEdge` | `EdgeRetraction` | a relationship stopped holding at `ValidEnd` |
| `ObserveChange` | `ChangeFact` | something *happened*: a rollout, a scaling, a config change |
| `IdentityClaim` | `IdentityFact` | "my source also knows this entity by that name" (§4) |
| `SourceCheckpoint` | `CheckpointFact` | the extent of what you have observed (§6) — prefer `Emitter.Checkpoint` |

### The taxonomy

`Type` and `Kind` are enums from the generated schema package, imported as
`graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"`. They are the taxonomy the
constitution fixes, so the list is short and closed. (`go doc` does not print generated enum
values under their type; `go doc -all` does, and
[`graph.proto`](../../api/sreagent/graph/v1/graph.proto) is the source of truth.)

| | |
|---|---|
| `graphv1.NodeType_` | `SERVICE` · `WORKLOAD` · `INFRA_RESOURCE` · `CONFIG` · `FEATURE_FLAG` · `DB_SCHEMA` · `THIRD_PARTY` · `OWNER` · `ALERT` · `CHANGE` |
| `graphv1.EdgeType_` | `CALLS` · `DEPENDS_ON` · `RUNS_ON` · `DEPLOYED_BY` · `OWNED_BY` · `EXPOSED_VIA` · `CHANGED_BY` |
| `graphv1.ChangeKind_` | `ROLLOUT` · `IAC_APPLY` · `FLAG_FLIP` · `SECRET_ROTATION` · `SCALING` · `MIGRATION` · `DNS_SWITCH` · `CLOUD_MAINTENANCE` · `CONFIG_CHANGE` · `CHANGE_KIND_OTHER` |

`CHANGED_BY` edges are not yours to emit: `ChangeFact.Targets` produces them. Use
`ChangeKind_CHANGE_KIND_OTHER` with `KindOther` set for a kind the schema does not name yet, and
open an issue — a kind that recurs belongs in the enum.

### Identifiers

```go
feeder.NewID("k8s:demo", "deploy", "shop/checkout@rv1001")
// "k8s:demo:deploy:shop/checkout@rv1001"
```

`NewID` joins the parts with `:` and sanitises characters that would make an id ambiguous. The
**parts are yours to choose and they must be a pure function of the payload.** Every conformance
check hangs off this: if a re-read of the same object produces the same id, re-delivery is a
no-op; if it does not, the graph accumulates duplicate versions and `testkit.DoubleDeliver`
fails.

`IdempotencyKey(eventID)` returns the event id itself, which is the default and what every
fixture uses. Pass extra parts only for the one case that needs it: the same fact re-asserted
under a new event id — a resync re-reading an object it already reported — that should collapse
onto the first delivery.

### Valid time

`ValidAt` is when the fact became true **in the production system**, as your source asserts it.
It is not when you noticed; the graph stamps that itself as observed time, and a feeder cannot
influence it.

If your source genuinely does not know when something started — a workload that already existed
when your first list call returned, a feature flag nobody has touched since it was created — set
`ValidFromUnknown: true`. Do not guess a timestamp: guessing is a defect (FR-011), and it also
makes your events depend on delivery order, which `testkit.Shuffle` will catch.

Set one or the other, never both. They are separate fields on the fact, so nothing stops you
filling in `ValidAt` *and* `ValidFromUnknown`; the graph then still treats the lower bound as
unknown, which is almost never what a feeder that had a timestamp meant to say. When the
timestamp is conditional, make the flag conditional on the same test:

```go
ValidAt:          fl.UpdatedAt,
ValidFromUnknown: fl.UpdatedAt.IsZero(),
```

### Properties

The `Props` builder has typed setters and no `Set(key string, value any)`, which is deliberate:
there is no convenient way to attach a slice of numbers or an arbitrary nested document — the
shapes telemetry arrives in. `Build` runs the graph's own validator over the result, so you get
the server's exact reason code at the point of the mistake.

```go
props, err := feeder.NewProps().
    Str(feeder.AttrK8sNamespaceName, "shop").      // OTel semconv where one exists
    Str(feeder.AttrK8sDeploymentName, "checkout").
    Str(feeder.AttrDeploymentEnvironment, "prod").
    Int(feeder.PropK8sReplicas, 3).                // sre.* for everything else
    Str(feeder.PropK8sResourceVersion, "1001").
    Build()
if err != nil {
    return err // a *feeder.Rejection naming the property and why
}
```

Property names follow **OpenTelemetry semantic conventions** wherever one exists — `service.name`,
`service.namespace`, `service.version`, `deployment.environment.name`, `k8s.namespace.name`,
`k8s.deployment.name`, `k8s.cluster.name`, `k8s.node.name`, `db.system`, `server.address`,
`server.port` — and the project namespace `sre.` everywhere else (`sre.k8s.revision`,
`sre.config.value_hash`, `sre.owner.kind`, `sre.window.seconds`, …). This is not tidiness: the
entity-resolution rules match on these names, so a connector that invents
`kubernetes_deployment` has written a fact no rule can read. Every constant is in
[`props.go`](../../pkg/feeder/props.go).

**What is refused**, with the reason code you will see:

| You wrote | Reason code |
|---|---|
| `metric_value`, `value`, `log_body`, `body`, `span_id`, `trace_id`, `samples` — at any depth | `telemetry_payload` |
| a list of two or more numbers, whatever it is called | `telemetry_payload` |
| a property over 4 KiB | `telemetry_payload` |
| a secret's material on a `sre.config.kind: secret` node | `secret_value` |
| no `ValidAt` and no `ValidFromUnknown` | `missing_valid_time` |
| an unspecified node, edge or change type | `unknown_type` |
| a ref missing its namespace or its value | `missing_ref` |

Call `feeder.Validate(ev)` yourself if you want to check an envelope before emitting it; every
emitter already does.

### Weight classes

`calls` edges carry traffic as a coarse class, never a rate — a rate is a measurement and
measurements live in the telemetry backend.

```go
class := feeder.WeightClassFromCount(spanCount, 5*time.Minute) // or feeder.WeightClass(rps)
edge := feeder.UpsertEdge(d, id, feeder.EdgeFact{
    Src: caller, Dst: callee, Type: graphv1.EdgeType_CALLS,
    WeightClass: feeder.WeightClassPtr(class),
    ValidAt:     windowStart,
})
```

`0` observed but negligible · `1` < 0.1 rps · `2` < 1 rps · `3` < 10 rps · `4` < 100 rps ·
`5` ≥ 100 rps. Emit a new edge version only when the class **changes**, and give it one full
window of hysteresis before believing the change. Leave `WeightClass` nil on every edge type
other than `calls`: "not applicable" and "class 0" are different statements.

---

## 4. Identity claims

A pod, a telemetry service name, a repository and a chat channel may be the same entity under
four names. Resolving them is the hard problem, and it is done in the open by the graph, never
by you.

**Emit an `IdentityClaim` for every external identifier you know about an entity, including the
one you address it by, and stop there.** Never merge two identities yourself: you have no way to
record why, and every merge must carry its rule, its confidence and its rationale.

```go
ev := feeder.IdentityClaim(d, feeder.NewID(d.SourceID, "claim", "shop/checkout@"+feeder.NSAppName),
    feeder.IdentityFact{
        Subject: feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
        Claim:   feeder.Ref(feeder.NSAppName, "checkout"),
        Attributes: attrs, // the supporting facts a rule needs
    })
```

`Attributes` is what makes a claim decidable. Include the environment
(`deployment.environment.name`) and the grouping (`k8s.namespace.name` or `service.namespace`) —
`checkout` in staging is not `checkout` in production — and, when the claim came from a label or
an annotation, `sre.k8s.claim_key` and `sre.k8s.claim_kind`, so a rule can tell an operator's
declaration from a coincidence.

Use the published namespaces so that another connector's claim can meet yours:

`otel.service.name` · `server.address` · `third_party` · `app.kubernetes.io/name` ·
`owner.team` · `k8s.cluster` · `k8s.namespace` · `k8s.nodepool` · `k8s.node` · `k8s.deployment` ·
`k8s.statefulset` · `k8s.daemonset` · `k8s.job` · `k8s.cronjob` · `k8s.service` · `k8s.ingress` ·
`k8s.configmap` · `k8s.secret` · `k8s.change` · `otel.change`

They are constants — `feeder.NSOTelService`, `feeder.NSK8sDeployment`, … — in
[`ref.go`](../../pkg/feeder/ref.go). Minting a new one for a system nobody has connected yet is
expected; reusing an existing one for something it does not mean is not.

---

## 5. Pointers

A pointer is the graph's answer to "show me the latency of this service around 14:32". It names
a backend, a vocabulary, a selector in that vocabulary, and the resource attributes that
identify the entity. It never carries the telemetry itself.

```go
attrs := map[string]string{
    feeder.AttrServiceName:           "checkout",
    feeder.AttrServiceNamespace:      "shop",
    feeder.AttrDeploymentEnvironment: "prod",
}
selector := feeder.OTelSelector(attrs, feeder.AttrServiceName, feeder.AttrServiceNamespace)
// service.name="checkout" AND service.namespace="shop"

pointers := []*graphv1.Pointer{
    feeder.TracePointer("tempo", selector, attrs),
    feeder.MetricPointer("prometheus",
        feeder.MetricSelector("http.server.request.duration", attrs,
            feeder.AttrServiceName, feeder.AttrServiceNamespace), attrs),
    feeder.LogPointer("loki", selector, attrs),
    feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource,
        "apps/v1/namespaces/shop/deployments/checkout", attrs),
}
```

`TracePointer`, `MetricPointer` and `LogPointer` all use the `otel-semconv/1.30` vocabulary, so
the backend behind them is interchangeable. `SourceLinkPointer` links back into the system you
read the entity from (`k8s-resource/v1` for a Kubernetes resource path). `DashboardPointer` and
the generic `NewPointer` cover the rest; a pointer that cannot be expressed in OpenTelemetry
terms must say in a comment why not.

`OTelSelector` renders the keys you name, in the order you name them, and sorts them when you
name none — so a selector is byte-stable across runs, which matters because goldens are compared
byte for byte.

---

## 6. Checkpoints

A checkpoint is how the graph tells "nothing happened" from "nobody was watching".

```go
err := em.Checkpoint(ctx, windowStart, windowEnd, gapBefore)
```

Emit one **on start, on resync, and after any gap**, with `gapBefore: true` whenever you were not
watching immediately before `from` — a restart, a dropped watch, an expired resource version.
`Emitter.Checkpoint` mints the deterministic event id for you
(`<source>:ckpt:<YYYYMMDDTHHMMSSZ>`), so two checkpoints covering the same instant are one event.

Compute the extent from the whole window, not from "the last payload I happened to see": the
latter depends on delivery order and `testkit.Shuffle` will fail you for it. Payloads whose
valid time you do not know (the `ValidFromUnknown` case above) contribute nothing to the extent —
skip them rather than folding a zero `time.Time` in, which would push `from` back to year 1. If
no payload in the run carried a timestamp there is no extent to assert, so emit no checkpoint.

---

## 7. Read-only, and proving it

**A feeder must use read-only credentials and must refuse to start without them (FR-046).** The
SDK cannot check this for you — only your source system knows what a scope grants — so:

1. Document the scopes you need in `Description.RequiredScopes`.
2. At the top of `Run`, ask the source system what your credential can do and return an error if
   it can write. Kubernetes has `SelfSubjectRulesReview`; most vendor APIs expose the token's
   scopes on a `/me`-style endpoint. If yours genuinely cannot tell you, say so in your
   connector's README and require the operator to assert it.
3. Never call a mutating endpoint, not even a dry-run one.

Your own credential for the graph is a **feeder-role token scoped to your `SourceID`**. The
server checks every event against it, so a compromised feeder can only lie about its own source.
Against a development server:

```sh
aisre serve --auth dev --dev &
export TOKEN=$(aisre dev-token --dev --user my-feeder --roles feeder --source-id myvendor:prod-eu1)
```

In production the token comes from your organisation's identity provider with the `feeder` role
and a `source_id` claim.

---

## 8. A complete feeder

This is the whole thing: a feeder over a source that emits one JSON document per workload. It
compiles as written.

```go
// SPDX-License-Identifier: Apache-2.0

package myfeeder

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "time"

    graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
    "github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// workload is one payload as the source system produces it.
type workload struct {
    Namespace       string    `json:"namespace"`
    Name            string    `json:"name"`
    AppName         string    `json:"appName"`
    Replicas        int64     `json:"replicas"`
    ResourceVersion string    `json:"resourceVersion"`
    ValidAt         time.Time `json:"validAt"`
}

// Feeder reads workloads from one cluster.
type Feeder struct {
    SourceID string
    Cluster  string
}

func (f *Feeder) Describe() feeder.Description {
    return feeder.Description{
        SourceID:         f.SourceID,
        Kind:             "myvendor",
        Ordering:         feeder.OrderingPerSourceSequence,
        ReorderingWindow: 60 * time.Second,
        RequiredScopes:   []string{"get,list,watch on apps/v1 deployments"},
        Namespaces:       []string{feeder.NSK8sCluster, feeder.NSK8sDeployment, feeder.NSAppName},
    }
}

func (f *Feeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
    d := f.Describe()
    if err := d.Validate(); err != nil {
        return err
    }
    // FR-046: prove the credential is read-only before emitting anything.
    if err := f.checkReadOnly(ctx); err != nil {
        return err
    }

    var first, last time.Time
    for {
        payload, err := src.Next(ctx)
        if errors.Is(err, io.EOF) {
            break
        }
        if err != nil {
            return err
        }

        var wl workload
        if err := json.Unmarshal(payload.Bytes, &wl); err != nil {
            return fmt.Errorf("myfeeder: decode payload: %w", err)
        }
        if err := f.emit(ctx, em, d, wl); err != nil {
            return err
        }

        // The extent is derived from the whole set, never from "the last one seen", so that
        // delivery order cannot change it.
        if first.IsZero() || wl.ValidAt.Before(first) {
            first = wl.ValidAt
        }
        if wl.ValidAt.After(last) {
            last = wl.ValidAt
        }
    }

    if !first.IsZero() {
        if err := em.Checkpoint(ctx, first, last, false); err != nil {
            return err
        }
    }
    return em.Flush(ctx) // always flush before returning
}

func (f *Feeder) emit(ctx context.Context, em feeder.Emitter, d feeder.Description, wl workload) error {
    ref := wl.Namespace + "/" + wl.Name
    rv := "@rv" + wl.ResourceVersion

    clusterProps, err := feeder.NewProps().
        Str(feeder.AttrK8sClusterName, f.Cluster).
        Str(feeder.AttrDeploymentEnvironment, "prod").
        Build()
    if err != nil {
        return err
    }
    workloadProps, err := feeder.NewProps().
        Str(feeder.AttrK8sClusterName, f.Cluster).
        Str(feeder.AttrK8sNamespaceName, wl.Namespace).
        Str(feeder.AttrK8sDeploymentName, wl.Name).
        Str(feeder.AttrDeploymentEnvironment, "prod").
        Str(feeder.PropK8sResourceVersion, wl.ResourceVersion).
        Int(feeder.PropK8sReplicas, wl.Replicas).
        Build()
    if err != nil {
        return err
    }
    claimAttrs, err := feeder.NewProps().
        Str(feeder.AttrK8sNamespaceName, wl.Namespace).
        Str(feeder.AttrDeploymentEnvironment, "prod").
        Str(feeder.PropK8sClaimKey, feeder.NSAppName).
        Str(feeder.PropK8sClaimKind, "label").
        Build()
    if err != nil {
        return err
    }

    logAttrs := map[string]string{
        feeder.AttrK8sNamespaceName:  wl.Namespace,
        feeder.AttrK8sDeploymentName: wl.Name,
    }

    events := []*graphv1.EventEnvelope{
        // The cluster is asserted by every payload, and its id is a pure function of the
        // cluster, so every assertion after the first is a DUPLICATE_NOOP. Its valid start is
        // genuinely unknown — the cluster existed before we looked — and asserting the arrival
        // time of whichever payload came first would make the event depend on delivery order.
        feeder.UpsertNode(d, feeder.NewID(d.SourceID, "cluster", f.Cluster), feeder.NodeFact{
            Ref:              feeder.Ref(feeder.NSK8sCluster, f.Cluster),
            Type:             graphv1.NodeType_INFRA_RESOURCE,
            DisplayName:      f.Cluster,
            Props:            clusterProps,
            ValidFromUnknown: true,
        }),
        feeder.UpsertNode(d, feeder.NewID(d.SourceID, "workload", ref+rv), feeder.NodeFact{
            Meta:        feeder.Meta{Seq: seqOf(wl.ResourceVersion)},
            Ref:         feeder.Ref(feeder.NSK8sDeployment, ref),
            Type:        graphv1.NodeType_WORKLOAD,
            DisplayName: wl.Name,
            Props:       workloadProps,
            Pointers: []*graphv1.Pointer{
                feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource,
                    "apps/v1/namespaces/"+wl.Namespace+"/deployments/"+wl.Name, logAttrs),
                feeder.LogPointer("loki", feeder.OTelSelector(logAttrs), logAttrs),
            },
            ValidAt: wl.ValidAt,
        }),
        feeder.UpsertEdge(d, feeder.NewID(d.SourceID, "edge", "runs-on", ref+rv), feeder.EdgeFact{
            Meta:    feeder.Meta{Seq: seqOf(wl.ResourceVersion)},
            Src:     feeder.Ref(feeder.NSK8sDeployment, ref),
            Dst:     feeder.Ref(feeder.NSK8sCluster, f.Cluster),
            Type:    graphv1.EdgeType_RUNS_ON,
            ValidAt: wl.ValidAt,
        }),
        // Claim, never merge: the graph decides whether this workload is the OTel service of
        // the same name.
        feeder.IdentityClaim(d, feeder.NewID(d.SourceID, "claim", ref+"@"+feeder.NSAppName),
            feeder.IdentityFact{
                Subject:    feeder.Ref(feeder.NSK8sDeployment, ref),
                Claim:      feeder.Ref(feeder.NSAppName, wl.AppName),
                Attributes: claimAttrs,
            }),
    }

    for _, ev := range events {
        result, err := em.Emit(ctx, ev)
        if err != nil {
            return err
        }
        // A REJECTED result is an answer, not a transport failure. Log it, count it, keep going.
        if result.GetStatus() == graphv1.IngestResult_REJECTED {
            return fmt.Errorf("myfeeder: %s refused: %s (%s)",
                ev.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
        }
    }
    return nil
}

// checkReadOnly asks the source system what this credential may do and refuses a writable one.
func (f *Feeder) checkReadOnly(context.Context) error { return nil /* your API call here */ }

func seqOf(resourceVersion string) int64 {
    var n int64
    for _, r := range resourceVersion {
        if r < '0' || r > '9' {
            return 0
        }
        n = n*10 + int64(r-'0')
    }
    return n
}
```

### Running it live

```go
em, err := emit.NewConnectEmitter(graphURL, token, f.Describe(),
    emit.WithBatchSize(100),
    emit.WithFlushInterval(time.Second),
    emit.WithResultFunc(func(r *graphv1.IngestResult) { /* your metrics */ }),
)
if err != nil {
    return err
}
defer em.Close(context.Background())

src := source.NewChanSource(128)
go watchYourSystem(ctx, src) // calls src.Push(ctx, feeder.Payload{...}); src.Close() when done

return f.Run(ctx, src, em)
```

With a batch size above 1, `Emit` returns a nil result because the graph has not answered yet —
register `WithResultFunc` to see every result, or set `WithBatchSize(1)` while you are writing
the feeder and want each answer inline. `ConnectEmitter` registers your source on first use,
retries transient transport failures with backoff, and surfaces every rejection through the
callback, a warning log and `Stats()`.

---

## 9. Recording a fixture

**A connector without a fixture is not merged.** A fixture is a directory holding the payloads
your source actually produced and the events they must produce; it is what makes your connector
testable by people who do not have your cluster.

**Record with a batch size of 1.** `record.Emitter` writes a line per event *the graph answered
for*, because the observed time it records is the one the graph assigned at acceptance (§8,
FR-023). A batching emitter has not asked yet and answers `nil`, so recording through one
produces payloads, an empty `events.jsonl` and a directory that looks like a finished fixture.
The SDK's default batch size is `emit.DefaultBatchSize` (100), so this is the case you get
unless you say otherwise; the live run of 2026-09-16 lost a cluster run to it. `record.Emitter`
now returns `record.ErrBatchingEmitter` from the first such event rather than writing nothing,
and `aisre feed k8s --record` forces `--batch-size=1` for you.

Run your feeder once with both recorders in place:

```go
dir := "fixtures/myvendor-topology-01"

// One answer per event, or the recorder has no observed time to write down.
connectEmitter, err := emit.NewConnectEmitter(server, token, f.Describe(), emit.WithBatchSize(1))

src := record.Wrap(liveSource, dir)                 // tees payloads   -> <dir>/payloads/
em  := record.Emitter(connectEmitter, dir)          // tees events     -> <dir>/events.jsonl

if err := f.Run(ctx, src, em); err != nil {
    return err
}
if err := src.Err(); err != nil { return err }
if err := em.Err(); err != nil { return err }

return record.WriteManifest(dir, record.Manifest{
    Family:         "myvendor-topology",
    Description:    "one cluster, five workloads, thirty minutes",
    Sources:        []record.ManifestSource{record.SourceOf(f.Describe())},
    ExpectRejected: em.Rejections(),
})
```

What comes out:

```text
fixtures/myvendor-topology-01/
├── manifest.yaml          # sources, clock, schema and SDK versions, expect_rejected
├── payloads/
│   ├── index.jsonl        # {"kind":…,"at":…,"seq":…,"file":"…"} in arrival order
│   ├── deployments/000001.json
│   └── deployments/000002.pb
├── events.jsonl           # accepted events, canonical JSON + observedAt + appendedSeq
└── rejected.jsonl         # events that must always be refused (only if there were any)
```

Each `events.jsonl` line is one `EventEnvelope` in canonical protobuf JSON — keys sorted, no
insignificant whitespace, 64-bit integers as strings — plus two fields the log adds:
`observedAt`, the observed time the graph assigned (replay uses it, never the replay clock), and
`appendedSeq`, the line number from 1. `payloads/` files are `.json` when the bytes are JSON and
`.pb` otherwise, numbered per kind so that a directory listing is also the replay order.

### Recording without a graph

`record.Emitter` tees any `feeder.Emitter`, not only a `ConnectEmitter`, so you can produce
`events.jsonl` from payloads you already have with no server, no database and no cluster. This
is the loop you actually want while writing the connector:

```go
src, err := source.NewFileSource(dir)              // replays <dir>/payloads/ in index order
em := record.Emitter(emit.NewMemoryEmitter(f.Describe()), dir)
if err := f.Run(ctx, src, em); err != nil { return err }
return em.Err()
```

One caveat, and it is the reason this is not how a fixture is *finished*: a memory emitter is
not the graph, so the `observedAt` it records is the recorder's own wall clock. Re-running
rewrites every line of `events.jsonl`, and the file is therefore not reproducible. Use it to
iterate; record the fixture you commit against a real graph (above), where `observedAt` is the
instant the log assigned and replay uses it rather than the replay clock (FR-023).

### The manifest

`record.WriteManifest` fills in the fields it can infer. The rest — the ones `aisre fixture
verify` reads — you write by hand; [`fixtures/README.md`](../../fixtures/README.md) is the full
reference and this is the minimum:

```yaml
id: myvendor-topology-01          # the directory name
family: myvendor-topology         # fixtures in a family share a shape
description: >-
  One cluster, five workloads, thirty minutes.
schema_version: 1.0.0             # feeder.SchemaVersion you emitted under
sdk_version: 0.1.0                # feeder.SDKVersion
hand_authored: false              # true if the payloads were written rather than recorded
events: events.jsonl

sources:
  - source_id: myvendor:prod-eu1
    kind: myvendor
    ordering: per_source_sequence
    reordering_window: 60s

clock:
  start: 2026-09-01T08:59:59Z     # one second before the first payload
  end:   2026-09-01T14:26:00Z     # the last payload

queries: []                       # see below: the reads this fixture pins
```

Then, by hand: **sanitise the recording** — only you know which host names are secrets — and add
a `queries:` list to `manifest.yaml` naming the reads whose answers the fixture should pin. The
manifest is written with a comment reminding you. `aisre fixture record <dir>` writes those
golden outputs and `aisre fixture verify <dir>` checks them for ever after.

---

## 10. Running the conformance suite

```go
package myfeeder_test

import (
    "testing"

    "example.com/my-feeder/myfeeder"
    "github.com/Pierre-Theophile/aisre/pkg/feeder/testkit"
)

func TestConformance(t *testing.T) {
    f := &myfeeder.Feeder{SourceID: "myvendor:prod-eu1", Cluster: "shop-prod"}
    testkit.Conformance(t, f, "testdata/myvendor-topology-01")
}
```

That is the whole test, it needs no database, and on a five-payload fixture it runs in well
under a second. Start it with `testkit.WithoutStreamComparison()` while `events.jsonl` is still
empty, and drop the option the moment you have recorded one — the stream comparison is the check
that actually pins your output.

It runs three checks, which you can also call individually:

- **`Run`** replays `payloads/` through your feeder and asserts: the description is well formed;
  every event validates against the published schema; nothing was refused; every ref is in a
  namespace you declared; `Flush` was called before `Run` returned; and the stream equals
  `events.jsonl` for your source id, compared as canonical JSON.
- **`Shuffle`** re-runs with the payloads permuted inside your declared `ReorderingWindow` and
  asserts you emitted the same set of events. This catches any fact derived from *arrival* order
  rather than from the payloads — a valid time taken from "the first payload I saw", an extent
  taken from "the last one".
- **`DoubleDeliver`** replays twice into the same graph and asserts every second delivery is
  `DUPLICATE_NOOP`. This catches a non-deterministic event id, which passes every other check.

No database is needed. If you want the checks to additionally prove a real graph accepts your
events, tee a second emitter in — inside this repository that is a projector over a test
database:

```go
testkit.Conformance(t, f, dir, testkit.WithEmitter(
    func(t *testing.T, d feeder.Description) testkit.ResultEmitter {
        return emit.NewProjectorEmitter(projector.New(pgtest.Open(t)), d)
    }))
```

Other options: `WithShuffles(n)`, `WithSeed(seed)`, `WithTimeout(d)`, and
`WithoutStreamComparison()` for a fixture whose `events.jsonl` has not been recorded yet.

For unit tests of a single handler, `emit.NewMemoryEmitter(desc)` gives you the whole rejection
vocabulary with no fixture at all: it validates every event exactly as the server would,
deduplicates by idempotency key, and reports `Events()`, `Results()` and `Rejected()`.

---

## 11. Versioning

- **The SDK** is semver: MINOR adds helpers, MAJOR changes the interfaces in
  [`feeder.go`](../../pkg/feeder/feeder.go). `feeder.SDKVersion` is recorded in every fixture's
  `sdk_version`.
- **The event schema** is versioned separately. `Description.SchemaVersion` pins the version you
  emit (`feeder.SchemaVersion`, currently `1.0.0`); `feeder.AcceptedSchemaVersions()` is what this
  build of the graph accepts. Emitting anything else gets every event refused with
  `unknown_schema_version`, and the rejection detail names the versions that would work.
- **Your connector** has its own semver. Fixtures record both it and the schema version, so a
  fixture recorded under an older schema stays replayable: a breaking schema change ships with a
  transformation rather than invalidating what you recorded.
- **Reason codes are part of the published schema.** You may match on them
  (`feeder.ReasonTelemetryPayload`, …); they change only with a version bump.

---

## 12. The rules, in one list

1. Never emit telemetry payloads — attach a pointer instead.
2. Use read-only credentials and refuse to start without them.
3. Derive event ids deterministically from source-native identifiers, never from a clock, a
   counter or a random source.
4. Emit an `identity_claim` for every external identifier you know about an entity, and let the
   graph resolve.
5. Emit a `source_checkpoint` on start, on resync, and after any gap.
6. Ship a fixture, or the connector is not merged.

Before you open the pull request, walk [checklist.md](./checklist.md): it is the same six rules
plus everything else a reviewer will check.

---

### Reference

| | |
|---|---|
| Public API | [`pkg/feeder`](../../pkg/feeder), [`emit`](../../pkg/feeder/emit), [`record`](../../pkg/feeder/record), [`source`](../../pkg/feeder/source), [`testkit`](../../pkg/feeder/testkit) |
| Event schema | [`api/sreagent/graph/v1/graph.proto`](../../api/sreagent/graph/v1/graph.proto) |
| Temporal model | [`docs/schema/temporal-model.md`](../schema/temporal-model.md) |
| Fixture format | [`fixtures/README.md`](../../fixtures/README.md) |
| A worked example | [`fixtures/baseline-topology-01`](../../fixtures/baseline-topology-01) |
| An MCP connector | [`docs/connectors/mcp.md`](./mcp.md), [`examples/mcp-feeder`](../../examples/mcp-feeder) |
