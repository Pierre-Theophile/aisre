<!-- SPDX-License-Identifier: Apache-2.0 -->

# The disposable cluster

Everything in this directory exists to produce a real Kubernetes cluster with real traced
traffic, so that the two reference feeders can be pointed at something that is not a fixture.
That is the second half of acceptance: FR-050 (b) asks for "a documented live run in which both
reference feeders are pointed at a disposable Kubernetes cluster running a demo application
with tracing enabled, and the resulting graph matches the graph produced from recordings of
that same run", and SC-010 says the same thing in terms of live-versus-recorded parity.

Concretely, this is where these come from:

| | |
|---|---|
| `fixtures/baseline-topology-01/payloads/k8s/` | T058 — recorded from the cluster below |
| `fixtures/baseline-topology-01/payloads/otel/` | T063 — recorded from the same run |
| `fixtures/config-change-01` | T066 — `scenarios/config-change.sh` |
| `fixtures/feeder-gap-01` | T067 — `scenarios/feeder-gap.sh` |
| `fixtures/rollout-regression-02` | T082 — the nightly job, `scenarios/rollout.sh` |
| quickstart §7 | the live run itself |

The cluster is meant to be destroyed. Nothing in it is configured for anything but that.

## Contents

| Path | What it is |
|---|---|
| `cluster.yaml` | the kind cluster: one control plane, one worker, node-pool labels, port mappings |
| `up.sh` / `down.sh` | create-and-deploy, and delete. Both idempotent |
| `host-endpoint.sh` | works out the address pods use to reach a process on your host, and writes it into both overlays |
| `otel-demo/minimal/` | a self-contained synthetic shop — this is what the fixtures are recorded from |
| `otel-demo/full/` | the real OpenTelemetry Demo, its collector teed to sre-agent |
| `otel-demo/fetch-upstream.sh` | downloads the pinned upstream demo manifest for `full/` |
| `scenarios/` | the scripted changes the fixtures are built around |
| `.github/workflows/e2e-nightly.yml` | everything below, run unattended every night — see [The nightly run](#the-nightly-run) |

## Prerequisites

- `kind` v0.33.0 or newer. The node image is pinned in `cluster.yaml` to
  `kindest/node:v1.37.0`, which is that release's default; a much newer kind will want a newer
  node image.
- `kubectl` v1.31 or newer (for `kubectl apply -k` and `kubectl kustomize`).
- Docker, running. The minimal overlay is happy with Docker's defaults; the full overlay wants
  roughly 6 GB of memory and 4 CPUs given to the Docker VM.
- `sre-agent` built: `make build` → `bin/aisre`.

## The short way

```sh
deploy/kind/up.sh                       # cluster + the minimal shop
kubectl apply -k deploy/kind/rbac       # the read-only credential feed k8s insists on
deploy/kind/rbac/kubeconfig.sh > /tmp/sre-agent-feeder.kubeconfig
bin/aisre feed otel --source-id=otel:kind --listen-grpc=:4317 --listen-http=:4318 \
                        --window=5m --batch-size=1 --record=/tmp/live/rec/otel &
bin/aisre feed k8s  --kubeconfig=/tmp/sre-agent-feeder.kubeconfig \
                        --context=sre-agent-feeder --source-id=k8s:kind \
                        --cluster-name=sre-agent-demo --namespaces=shop --owner-labels=team \
                        --batch-size=1 --record=/tmp/live/rec/k8s &
deploy/kind/scenarios/rollout.sh
deploy/kind/down.sh
```

`--batch-size=1` is what makes the recording contain events; see [Recording](#recording).

The rest of this file is the same thing, slowly.

## Creating the cluster

```sh
kind create cluster --config deploy/kind/cluster.yaml
kubectl config use-context kind-sre-agent-demo
kubectl get nodes -o custom-columns='NAME:.metadata.name,POOL:.metadata.labels.sre\.node_pool'
```

The cluster is named `sre-agent-demo`, so kind writes the kubeconfig context
`kind-sre-agent-demo` — kind always prefixes the cluster name with `kind-` and has no setting to
change that. Every script here uses that context and honours `KUBE_CONTEXT` if you need another.

Two nodes, labelled so the Kubernetes feeder has a node pool to hang `runs-on` edges on:

| Node | `sre.node_pool` | `node-pool` |
|---|---|---|
| `sre-agent-demo-control-plane` | `control-plane` | `control-plane` |
| `sre-agent-demo-worker` | `general` | `general` |

Both spellings are set on purpose. `sre.node_pool` is the key the feeder reads by default;
`node-pool` is the key the pointer selectors in `fixtures/baseline-topology-01` use
(`v1/nodes?labelSelector=node-pool%3Dgeneral`). Setting both means a recording lines up with the
hand-authored fixture whichever key the feeder is configured with.

`extraPortMappings` publish node ports 30080 and 30443 on `127.0.0.1:8080` and `127.0.0.1:8443`.
Nothing serves them by default — see [The storefront Ingress](#the-storefront-ingress).

## Reaching the host from inside the cluster

This is the one genuinely fiddly part, so it gets its own section.

`aisre feed otel` runs **on your host** and listens on `:4317` (OTLP/gRPC) and `:4318`
(OTLP/HTTP). The pods have to reach out to it. That is the outbound direction, and kind's
`extraPortMappings` are the inbound one — host into the cluster. They do not help here, and no
kind setting does; what you need is an address.

Which address depends on how Docker runs:

| Platform | Address | Why |
|---|---|---|
| Docker Desktop (macOS, Windows) | `host.docker.internal` | Docker Desktop injects it into every container's `/etc/hosts`, pointing at an address its VM routes to the host |
| Docker Engine on Linux | the gateway of the `kind` bridge network, usually `172.18.0.1` | pods route to the node, the node routes to its bridge gateway, which is the host |

And the *name* cannot be used directly from a pod. kind rewrites each node's `/etc/resolv.conf`
away from Docker's embedded resolver — it has to, since `127.0.0.11` inside a pod is the pod's
own loopback — so `host.docker.internal` does not resolve in a pod even on Docker Desktop.

So the name is resolved on the host side and the literal is handed to the pods in a ConfigMap:

```sh
deploy/kind/host-endpoint.sh --print     # just show me the address
deploy/kind/host-endpoint.sh             # write it into both overlays, apply it, roll the readers
```

It resolves `host.docker.internal` from *inside* the node container (`docker exec … getent
hosts`), falls back to the `kind` network's gateway, and falls back again to `172.18.0.1`. The
result lands in `ConfigMap/sre-agent-otlp`:

```yaml
data:
  OTLP_HOST: "172.18.0.1"
  OTLP_GRPC_ENDPOINT: "172.18.0.1:4317"
  OTLP_HTTP_ENDPOINT: "http://172.18.0.1:4318"
```

There is one copy per overlay (`otel-demo/minimal/host-endpoint.yaml`,
`otel-demo/full/host-endpoint.yaml`) and the script writes both. The committed value is the
Linux default, so on Linux you can usually skip the script; on Docker Desktop you cannot.

Consumers pick it up as environment variables:

- the telemetrygen emitters take it with `envFrom` and expand `$(OTLP_GRPC_ENDPOINT)` in their
  args — Kubernetes does that substitution, telemetrygen never sees a `$`;
- the upstream collector in the full overlay takes it the same way and refers to it as
  `${env:OTLP_GRPC_ENDPOINT}` in its own configuration.

Because a ConfigMap consumed as environment variables is read once at container start, the
script restarts whatever reads it after changing it. **Re-run `host-endpoint.sh` after any
`kubectl apply -k`**, which would otherwise put the committed default back.

Last thing: make sure the feeder is actually listening on all interfaces, not just loopback.
`--listen-grpc=:4317` (rather than `127.0.0.1:4317`) is what the flag above does.

## The two overlays

| | `minimal` | `full` |
|---|---|---|
| What | a synthetic shop authored here | the real OpenTelemetry Demo, ~20 services |
| Spans from | telemetrygen, one container per edge | genuinely instrumented services |
| Needs a download | no | yes, `fetch-upstream.sh` |
| Weight | ~20 pods, a few hundred MB | ~25 pods, several GB |
| Namespaces | `shop`, `shop-traffic` | `otel-demo` |
| Use it for | recording fixtures, the nightly run | checking the feeder against real-world span shapes |

The fixtures are recorded from `minimal`. It is deliberately the same shape as
`fixtures/baseline-topology-01`, which makes the recorded fixture comparable to the
hand-authored one line by line (T058: "verify diff vs hand-authored version is only additive").

### Deploying `minimal`

```sh
kubectl apply -k deploy/kind/otel-demo/minimal --context kind-sre-agent-demo
deploy/kind/host-endpoint.sh
kubectl -n shop get all
kubectl -n shop-traffic get pods
```

What it contains:

| Object | Kind | Notes |
|---|---|---|
| `shop/storefront` | Deployment ×3 | `team=team-checkout`, Service, Ingress |
| `shop/checkout` | Deployment ×3 | `team=team-checkout`, Service, `envFrom` `checkout-config` |
| `shop/payments` | Deployment ×3 | `team=team-payments`, Service, mounts `payments-secret` |
| `shop/inventory` | Deployment ×3 | `team=team-payments`, Service |
| `shop/redis` | StatefulSet ×1 | headless Service, so `redis.shop.svc.cluster.local` resolves |
| `shop/payments-db` | Deployment ×1 | PostgreSQL, Service on 5432 |
| `shop/checkout-config` | ConfigMap | the thing `config-change.sh` edits |
| `shop/payments-secret` | Secret | a dummy value; the feeder records only its version |
| `shop-traffic/traffic-*` | 4 Deployments | 8 telemetrygen containers, one per edge |

Every app Deployment carries `app.kubernetes.io/name=<name>`, a `team` label, and the annotation
`resource.opentelemetry.io/service.name=<name>` — that annotation is the bridge that lets the
Kubernetes feeder claim `otel.service.name=checkout` for `k8s.deployment=shop/checkout`, so the
workload and the traced service resolve to one entity.

The graph it should produce:

```
storefront ──calls──▶ checkout ──calls──▶ payments ──calls──▶ payments-db  (postgresql)
                          │                   └────calls────▶ api.stripe.com
                          └──calls──▶ inventory
     checkout, payments, inventory ──calls──▶ redis
```

Images, all pinned:

| Image | Used by |
|---|---|
| `nginx:1.31.5-alpine-slim` | the four app workloads |
| `redis:8.10.1-alpine` | redis |
| `postgres:17.11-alpine` | payments-db |
| `ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.161.0` | the span emitters |

### Deploying `full`

```sh
deploy/kind/otel-demo/fetch-upstream.sh
kubectl apply -k deploy/kind/otel-demo/full --context kind-sre-agent-demo
deploy/kind/host-endpoint.sh
kubectl -n otel-demo rollout status deployment/otel-collector
```

The upstream manifest is **fetched, not vendored**: 20k lines of generated YAML in a repository
makes every diff unreadable. `fetch-upstream.sh` downloads
`kubernetes/opentelemetry-demo.yaml` from the pinned release, verifies a SHA-256, and writes it
to `otel-demo/full/upstream/`, which is git-ignored. Without that step `kubectl kustomize
deploy/kind/otel-demo/full` fails with `no such file or directory:
upstream/opentelemetry-demo.yaml`, which is the intended, legible failure.

Pinned to **opentelemetry-demo 2.2.0** (collector image
`otel/opentelemetry-collector-contrib:0.135.0`), SHA-256
`6d7e60bf1ba71a68d79ab3cac39482ab46627759ddcfcdb4e6a1d0645092ca3c`. 2.2.0 is the last release
that ships a rendered Kubernetes manifest — 3.0.0 removed `kubernetes/` in favour of the Helm
chart alone, so moving past it means rendering the chart yourself.

The overlay makes two changes, and only two:

1. `collector-config.yaml` replaces the collector's configuration with the identical upstream
   configuration plus an `otlp/sre-agent` exporter pointed at `${env:OTLP_GRPC_ENDPOINT}`, added
   to the traces pipeline alongside `otlp` (Jaeger), `debug` and `spanmetrics`. The whole
   configuration lives in one ConfigMap key as a YAML string, and there is no way to edit part
   of a string with kustomize, so the key is replaced wholesale. That header has the recipe for
   regenerating it after a version bump.
2. `collector-deployment.yaml` adds `envFrom: sre-agent-otlp` to the collector container, which
   is where that environment variable comes from.

Jaeger keeps receiving traces, so the demo's own UI still works while sre-agent is fed.

## Verifying that spans arrive

Start the OTLP feeder in the foreground first, before anything else, and watch it:

```sh
bin/aisre feed otel --source-id=otel:kind --listen-grpc=:4317 --listen-http=:4318 \
                        --window=5m --record=/tmp/live/otel
```

Then, in another shell:

```sh
# 1. Are the emitters running and not crash-looping?
kubectl -n shop-traffic get pods
kubectl -n shop-traffic logs deployment/traffic-checkout -c to-payments --tail=20

# 2. Is the endpoint they were given the one you expect?
kubectl -n shop-traffic get configmap sre-agent-otlp -o jsonpath='{.data.OTLP_GRPC_ENDPOINT}'; echo

# 3. Is anything on the wire? Something has to be listening before the emitters start, or they
#    log export failures (and keep going, because of --allow-export-failures).
lsof -nP -iTCP:4317 -sTCP:LISTEN

# 4. Wait for one aggregation window (--window=5m), then ask the graph:
bin/aisre query subgraph otel.service.name=checkout --as-of now --hops 2
bin/aisre extent
```

A healthy `traffic-checkout` container logs a line per reporting interval and no export errors.
The most common failure is a `connection refused` in those logs, which means the address in
`sre-agent-otlp` is not reachable from the pod — go back to
[Reaching the host from inside the cluster](#reaching-the-host-from-inside-the-cluster) and
re-run `host-endpoint.sh`.

The subgraph should show `checkout` with `calls` edges to `payments`, `inventory` and `redis`,
and second-hop `payments-db` and `api.stripe.com`.

## Recording

Both feeders take `--record=<dir>`, which writes the raw source payloads plus the events they
emitted, in the layout `fixtures/README.md` documents. Run them together so that the two
recordings cover the same wall-clock window:

```sh
mkdir -p /tmp/live
bin/aisre feed otel --source-id=otel:kind --listen-grpc=:4317 --listen-http=:4318 \
                        --window=5m --batch-size=1 --record=/tmp/live/rec/otel &
bin/aisre feed k8s  --kubeconfig=/tmp/sre-agent-feeder.kubeconfig \
                        --context=sre-agent-feeder --source-id=k8s:kind \
                        --cluster-name=sre-agent-demo --namespaces=shop --owner-labels=team \
                        --batch-size=1 --record=/tmp/live/rec/k8s &
```

Notes that matter for the recording being usable:

- **`--batch-size=1` is not optional when recording.** The event recorder writes a line for
  each event the graph *answered for*, and a batching emitter answers later — so with the
  default `--batch-size=100` it has nothing to write and `events.jsonl` is never created, while
  `payloads/` fills up normally (`pkg/feeder/record/events.go`). A recording without events is
  not a fixture, and nothing warns you. One HTTP call per event is fine at these volumes.
- **Record each run into an empty directory.** `--record` into a directory that already holds
  a recording corrupts it: `events.jsonl` is appended to, but payload files are numbered from
  `000001` again and overwrite the earlier ones while the index still points at them
  (`pkg/feeder/record/payloads.go`). The feeder-gap scenario restarts the Kubernetes feeder
  mid-run, so record the reconnection into a second directory and merge the two afterwards.
- **A fresh graph per recording.** A feeder's event ids are pure functions of the objects they
  describe, so replaying the same cluster into a graph that already holds those events yields
  `DUPLICATE_NOOP` — which the recorder correctly does not write down, leaving a recording with
  the payloads but not the events they produced. Give each recording its own database.

- **Scope the Kubernetes feeder if you want the tightest fixture.** `--namespaces shop` (if the
  feeder offers it) or a later edit keeps `shop-traffic` out. The emitters are not part of the
  shop; see below.
- **Wait for at least two aggregation windows** before triggering a scenario, so the baseline is
  established and the change is unambiguous.
- `aisre feed k8s` refuses to start against credentials that grant write access (FR-046).
  kind's default kubeconfig is cluster-admin, so use a read-only ServiceAccount when the feeder
  enforces it.

The two recordings are then assembled into one fixture directory with `fixture assemble`:
`payloads/` merged and renumbered per kind (the two feeders' payload kinds never collide, and
the command refuses the merge if they ever do), `events.jsonl` ordered by observed time with
`appendedSeq` renumbered across the result, and a `manifest.yaml` carrying the union of the
sources each run declared. A restarted feeder's second directory goes on the same command line.

```sh
bin/aisre fixture assemble --out fixtures/my-fixture-01 --family rollout-regression \
  /tmp/live/rec/k8s /tmp/live/rec/k8s2 /tmp/live/rec/otel
```

What it deliberately does not write is the half a human decides: the description, the `queries:`
whose answers become the goldens, and the `ground_truth:` they are measured against. Add those,
then record and verify:

```sh
bin/aisre fixture record fixtures/baseline-topology-01 --db "$PG_DSN"   # writes golden/
bin/aisre fixture verify fixtures/baseline-topology-01 --db "$PG_DSN" --report
```

`fixture record` writes `golden/` — and `golden/pinned/`, the same queries with observed time
pinned to `clock.end`, which `fixture verify` requires (US7). It does **not** replay
`payloads/`: those are the evidence, and what replays them is the feeders' own conformance
tests (`internal/feeders/*/feeder_test.go`).

`make verify` runs the whole shipped set the way CI does:

```sh
make verify                                        # fixtures/*/ --report --db $PG_DSN
make verify FIXTURES=fixtures/baseline-topology-01  # just one
```

## The scenarios

Each script prints what it is about to do, uses `set -euo pipefail`, and is safe to run twice.
All of them honour `KUBE_CONTEXT` and `SHOP_NAMESPACE`.

| Script | What it changes | What the graph should get |
|---|---|---|
| `scenarios/rollout.sh` | `kubectl set image` on `shop/payments` (toggles between two pinned nginx patch releases), a `kubernetes.io/change-cause` annotation, and the `service.version` the emitters report | a rollout change node from the Kubernetes feeder (revision change, with an actor) *and* one from the OTLP feeder (`service.version` transition) |
| `scenarios/config-change.sh` | one key of `shop/checkout-config` | a `config_change` change node and a new CONFIG version with a new value hash — and **no** rollout, because the pods are deliberately not restarted |
| `scenarios/scale.sh` | replicas on `shop/inventory` (toggles 3 ↔ 5) | a `scaling` change node, no rollout |
| `scenarios/feeder-gap.sh` | stops the k8s feeder, deletes `svc/checkout`, waits, restarts the feeder **into the absence**, and only then restores the Service | a retraction whose `valid_end` is inside the gap and whose observed time is at reconnection, and the gap itself in `aisre extent` |

```sh
deploy/kind/scenarios/rollout.sh
deploy/kind/scenarios/rollout.sh --image nginx:1.31.4-alpine-slim --version 1.3.9
deploy/kind/scenarios/config-change.sh
deploy/kind/scenarios/config-change.sh --key FEATURE_FLAGS --value express-checkout,gift-wrap
deploy/kind/scenarios/scale.sh --deployment checkout --replicas 1
deploy/kind/scenarios/feeder-gap.sh --gap 300 --service storefront
```

`feeder-gap.sh` is the only one that touches a process. By default it finds a running process
whose command line matches `aisre feed k8s`, remembers that command line verbatim, sends it
`SIGTERM`, and starts the same command line again after the gap with its output going to a log
file it names. `--keep-feeder` turns that off and prompts you to do it yourself. It never kills
anything it did not match and never invents feeder arguments.

After any scenario, give the OTLP feeder an aggregation window and then:

```sh
bin/aisre query diff otel.service.name=checkout --from -30m --to now
```

## The span emitters, and what telemetrygen can and cannot do

The four nginx pods in `shop` do not talk to each other. Nothing in the minimal overlay makes a
real request: the `calls` half of the graph comes from **telemetrygen**, the span generator that
ships with `opentelemetry-collector-contrib`. It is a single static binary in a small image,
which is the whole reason it is here rather than a collector (too heavy) or hand-written
instrumentation (this task writes no code).

The OTLP topology feeder derives one edge per `(service, peer.service | db.system +
server.address | http host)` pair per window (research §11). telemetrygen gives exactly two
levers, and they map onto that cleanly:

| Flag | Scope | Used for |
|---|---|---|
| `--otlp-attributes k="v"` | **resource** attributes, set once for the process | the caller: `service.name`, `service.namespace`, `service.version`, `deployment.environment.name`, `k8s.namespace.name`, `k8s.deployment.name`, `k8s.cluster.name` |
| `--telemetry-attributes k="v"` | **span** attributes, set on every span | the callee: `peer.service`, or `db.system` + `server.address` + `server.port` for a third party |

Other flags used here: `--otlp-endpoint` (`host:4317`), `--otlp-insecure`, `--rate` (spans per
second per worker — a trace is two spans, and the rates here are `0.5` and `0.05`, chosen to
straddle a weight-class boundary at a volume a recorded fixture can carry; `traffic.yaml` has
the measurements),
`--duration=inf`, `--workers`, `--span-duration`, and `--allow-export-failures`. Values are
parsed as `key="string"`, `key=true|false`, `key=<integer>` or `key=[...]`, so the quotes in
`service.name="checkout"` are required and `server.port=5432` is genuinely an integer.

Three real constraints, and how they are handled:

1. **The caller is a resource attribute, so one process can only ever be one service.** A
   fan-out cannot come from one container. `checkout` has three callees, so `traffic-checkout`
   runs three telemetrygen containers, all declaring `service.name="checkout"` and each with a
   different `peer.service`. This is the reason the file looks repetitive: it is not style, it
   is the only way telemetrygen can express an edge.

2. **Span attributes are set on every span of a run, so the callee attribute lands on both spans
   of each generated trace** — the `lets-go` client span and its `okey-dokey-0` server child.
   The feeder aggregates per window, so the duplicate collapses into the one edge; it is noise,
   not error. `--child-spans=0` would remove it, but telemetrygen v0.161.0 panics in that case
   (`worker.go` passes an unset `SpanEndOption` to `span.End` when no child span was generated),
   so the default of 1 is kept.

3. **telemetrygen sets `peer.service="telemetrygen-server"` on the parent span itself**, before
   applying `--telemetry-attributes`. Later `SetAttributes` calls win, so
   `--telemetry-attributes peer.service="payments"` does override it — but that ordering is an
   implementation detail of telemetrygen, and it is the one thing to re-check when bumping the
   image. If a future version stops honouring the override, the fallback is the `full` overlay,
   whose spans come from genuinely instrumented services.

What telemetrygen does **not** do, and what nothing here pretends it does: propagate context
between services. Each emitter produces its own traces; there is no single trace threading
storefront → checkout → payments. The topology feeder reads attributes and never assembles
traces (research §11), so this costs nothing — but any future work that needs real trace
assembly has to use the `full` overlay.

### Why the emitters are in `shop-traffic`, not `shop`

They claim to be the shop services in their OTLP resource attributes; they are not the shop.
Keeping them in another namespace means `shop` contains exactly the six workloads of
`fixtures/baseline-topology-01` and nothing else, so a recording scoped to `shop` is directly
comparable to the hand-authored fixture.

Note also what those Deployments deliberately *lack*: no `resource.opentelemetry.io/service.name`
annotation and no `team` label. Annotating them would make the Kubernetes feeder claim
`otel.service.name=checkout` for `shop-traffic/traffic-checkout` as well as for `shop/checkout`
— an ambiguous identity, which is a fixture in its own right and not something to create here by
accident.

## Why these exact labels

The minimal overlay mirrors `fixtures/baseline-topology-01` as closely as a real cluster can, so
that T058's "diff vs hand-authored version is only additive" holds. The decisions worth knowing:

| Choice | Why |
|---|---|
| `storefront` is `team=team-checkout` | that is what the fixture records. A `team-storefront` would be a non-additive change to the owner graph |
| `inventory` is `team=team-payments` | likewise |
| replicas are 3 | the fixture records `sre.k8s.replicas: 3` |
| `checkout-config` is consumed with `envFrom`, `payments-secret` as a volume | the fixture records `sre.k8s.reference: envFrom` and `volume` on the two `depends-on` edges |
| `redis` has a **headless** Service | it is what makes `redis.shop.svc.cluster.local` the address the emitters claim, matching the THIRD_PARTY node |
| `payments-db` is a Deployment, `redis` a StatefulSet | FR-043 wants more than one workload kind exercised |
| the Ingress host is `shop.example.com` | the fixture records `server.address: shop.example.com` on the ingress node |
| service versions start at 3.1.0 / 2.3.1 / 1.4.0 / 0.9.7 | the `service.version` values in the fixture, held in `ConfigMap/shop-versions` |

Two things deliberately differ from the fixture and will show up in a recording: the cluster is
named `sre-agent-demo`, not `shop-prod`, and the `general` node pool has one node, not six.

## The storefront Ingress

There is no ingress controller in this cluster and nothing serves `shop.example.com`. The
Ingress object exists so the Kubernetes feeder emits an `exposed-via` edge to an INFRA_RESOURCE
carrying `server.address=shop.example.com` — which is what the fixture records. If you want it
to actually serve, install ingress-nginx's kind manifest; `cluster.yaml` already maps node ports
30080 and 30443 to `127.0.0.1:8080` and `127.0.0.1:8443` for exactly that.

## The nightly run

Everything in this file is also a GitHub Actions job:
[`.github/workflows/e2e-nightly.yml`](../../.github/workflows/e2e-nightly.yml), on a schedule and
on `workflow_dispatch`. It is the only job in the repository that needs a cluster, and it exists
because a recording nobody re-makes is a recording that silently stops describing the feeders
(FR-050 (b), SC-010, T082).

What it does, and where each step comes from in this file:

| Step | This file |
|---|---|
| install kind and kubectl, `make build` | [Prerequisites](#prerequisites) |
| `deploy/kind/up.sh minimal` | [Creating the cluster](#creating-the-cluster) |
| `host-endpoint.sh` | [Reaching the host from inside the cluster](#reaching-the-host-from-inside-the-cluster) |
| `kubectl apply -k deploy/kind/rbac` + `rbac/kubeconfig.sh` | the read-only credential `feed k8s` insists on (FR-046) |
| `kubectl apply -k deploy/kind/otel-demo/minimal`, wait for the pods, re-run `host-endpoint.sh` | [Deploying `minimal`](#deploying-minimal) |
| `serve` on its own database, then both feeders with `--batch-size=1 --record` | [Recording](#recording) |
| wait ≥ 12 minutes | two aggregation windows plus the lateness allowance, so the baseline is not one window |
| `scenarios/rollout.sh` | [The scenarios](#the-scenarios) |
| wait ≥ 11 minutes | the window the rollout falls in, the one after it, and the allowance |
| stop the feeders, `fixture assemble`, `fixture record`, `fixture verify --report` | [Recording](#recording) |
| `fixture export --db <live> --compare` against the replayed goldens | SC-010, below |
| upload the recording as an artifact, `down.sh` | [Tearing down](#tearing-down) |

Two things are worth knowing before reading a failed run.

**It runs on Linux, and this file has only ever been executed on Docker Desktop.**
`host-endpoint.sh` has two branches: on Docker Desktop it resolves `host.docker.internal` from
inside the node container, and on Docker Engine it falls through to the `kind` bridge network's
gateway — `172.18.0.1`, which is the committed default in both overlays. The nightly job takes
the second branch, runs the script explicitly anyway and prints its answer, because a wrong
address here is silent: the emitters keep going, log export failures, and the graph is simply
empty.

**SC-010 is a file comparison.** `fixture record` replays the recording into a database of its
own and writes `golden/`; `fixture export` runs the same manifest's queries against the database
the feeders were writing to *as it happened*, in the same canonical serialization, into a
directory laid out like `golden/`. Then:

```sh
bin/aisre fixture export --db "$LIVE_DSN" \
  --manifest /tmp/live/<fixture>/manifest.yaml --out /tmp/live-export \
  --compare /tmp/live/<fixture>
diff -r /tmp/live-export /tmp/live/<fixture>/golden
```

Silence means the graph built live and the graph rebuilt from the recording are the same graph —
entity ids, valid intervals, observed times and the extent block included. Not "the same up to
timestamps": the same bytes. `--compare` makes the same comparison in-process and exits 4 when
they differ, which is what the job gates on; `diff -r` is run too, because a person reading a
failed job wants the files.

The recording is uploaded as an artifact whatever the outcome, and promoting one into the
repository is a copy, a description, a `queries:` list and a pull request. That is where
`fixtures/rollout-regression-02` came from.

To run the same thing by hand, the job's steps are the sections above in order; to run it in CI
on demand, use **Actions → e2e-nightly → Run workflow**, which takes the two wait times as
inputs so a debugging run does not cost twenty-three minutes.

## Tearing down

```sh
deploy/kind/down.sh
# or
kind delete cluster --name sre-agent-demo
```

That removes the cluster, its containers and its kubeconfig context. Recordings written by
`--record` are untouched. To drop just the workloads and keep the cluster:

```sh
kubectl delete -k deploy/kind/otel-demo/minimal --context kind-sre-agent-demo
```

## Troubleshooting

| Symptom | Cause |
|---|---|
| Emitter logs show `connection refused` on `:4317` | the address in `sre-agent-otlp` is not reachable from the pod, or nothing is listening on the host. Re-run `host-endpoint.sh`, then check `lsof -nP -iTCP:4317 -sTCP:LISTEN` |
| Emitters were fine, then stopped exporting after `kubectl apply -k` | the apply put the committed default endpoint back. Re-run `host-endpoint.sh` |
| `kubectl kustomize .../full` fails on `upstream/opentelemetry-demo.yaml` | run `otel-demo/fetch-upstream.sh` first |
| Pods stuck in `CreateContainerConfigError` | `ConfigMap/sre-agent-otlp` or `ConfigMap/shop-versions` is missing from that namespace; re-apply the overlay |
| The full overlay's pods are `Pending` or `OOMKilled` | Docker has too little memory. The demo wants ~6 GB and 4 CPUs |
| `kind create cluster` fails on the node image | `cluster.yaml` pins `kindest/node:v1.37.0`, the default of kind v0.33.0. A different kind release wants its own default |
| A rollout produced no change node | the image was already the target; `rollout.sh` says so rather than creating an empty revision |

## Validating the manifests without a cluster

Both overlays render offline, which is the check to run before touching Docker:

```sh
kubectl kustomize deploy/kind/otel-demo/minimal >/dev/null && echo minimal ok
deploy/kind/otel-demo/fetch-upstream.sh
kubectl kustomize deploy/kind/otel-demo/full >/dev/null && echo full ok
bash -n deploy/kind/*.sh deploy/kind/scenarios/*.sh deploy/kind/otel-demo/*.sh
python3 -c 'import yaml,sys; yaml.safe_load(open(".github/workflows/e2e-nightly.yml"))' \
  && echo "nightly workflow ok"
```

What that cannot tell you, and what only a real run can: that the images pull, that the host
endpoint is reachable from a pod, that telemetrygen's flags behave as documented, that the
collector accepts the patched configuration, and that the two feeders produce matching live and
recorded graphs. That last one is the point of the whole directory.
