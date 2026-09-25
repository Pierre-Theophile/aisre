# Quickstart: GCP Integration and Vendor-Notice Feeder

**Feature**: 003-gcp-integration | **Date**: 2026-09-20 | **Phase**: 1

Runnable validation, in the order a contributor meets it. Every scenario here is something CI runs
or something a named person does once; none of it is illustrative. Implementation bodies belong in
`tasks.md`, not here.

**The load-bearing property of this page**: scenarios 1–6 need **no GCP account and no mailbox**.
They run against the synthetic structural twins in this repository. Only scenarios 7–10 touch the
organisation, and each says who has to be present for it.

---

## 0. Prerequisites

```bash
make build                       # → bin/aisre, static, CGO_ENABLED=0
export PG_DSN=...                # the Postgres 001 already requires; role needs CREATEDB
bin/aisre migrate

# The query commands in §2–§4 speak to a server. `--listen 127.0.0.1:…` is required rather than
# optional: `--auth dev` on a non-loopback address is refused, because the dev signing key is
# published.
bin/aisre serve --db "$PG_DSN" --auth dev --dev --listen 127.0.0.1:8080 &
export SRE_AGENT_TOKEN=$(bin/aisre dev-token --dev --user you --roles reader,decider)
```

Nothing else. No GCP credential, no mailbox, no network.

---

## 1. Replay the synthetic twins — the gate every PR must pass

```bash
bin/aisre fixture verify fixtures/gcp-*/ fixtures/vendor-*/ --report
```

Expected, per fixture, with **zero tolerance** (FR-142, SC-017):

| assertion | meaning |
|---|---|
| `replay: ok` | replayed from empty to its golden graph |
| `double-delivery: ok` | every event double-delivered as a `DUPLICATE_NOOP` |
| `shuffle: ok` | permuted inside the declared reordering window; no valid-time state moved |
| `expect-rejected: ok` | `gcp-telemetry-rejection-01` had its telemetry-carrying event **refused** |

A regression on any golden fails CI. `--report` adds the rows §6 reads.

---

## 2. The announced fact — the thing this feature exists to prove

The pattern the coverage audit found most often was *a vendor notice nobody read*. This scenario is
that pattern, defeated, and it is the one to run first when reviewing the feature.

```bash
bin/aisre fixture verify fixtures/vendor-maintenance-future-01/
```

Then ask the graph the three questions that distinguish a bitemporal store from a log:

```bash
# (a) The window, as known after the announcement → the change is there.
bin/aisre query subgraph 'vendor=payments-provider' \
    --as-of 2026-10-02T03:00:00Z --observed-at 2026-09-18T00:00:00Z --hops 2

# (b) The same question as known BEFORE the announcement → it is not.
bin/aisre query subgraph 'vendor=payments-provider' \
    --as-of 2026-10-02T03:00:00Z --observed-at 2026-09-16T00:00:00Z --hops 2

# (c) An incident on 18 September → the announced window is NOT a candidate cause,
#     and is excluded because its valid start is after the reference instant.
bin/aisre query diff 'gcp.cloudrun.service=<svc>' \
    --from 2026-09-18T01:00:00Z --to 2026-09-18T03:00:00Z --reference 2026-09-18T02:10:00Z
```

Expected: (a) returns the change with its announcement date visible; (b) does not; (c) does **not**
list it, and where it appears in a forward query it is marked `post_reference` — excluded **for that
stated reason**, not by scoring low (FR-065, SC-007).

> Reproduced 2026-09-22: (a) and (b) hold exactly. In (c) the change is excluded, but over every
> window tried it is excluded by being **absent from the candidate set** — the reason printed is
> "no change events in this window touched the neighbourhood" rather than `post_reference`. The flag
> and its post-reference temporal weighting are published in the ranking explanation the same command
> prints, so the mechanism exists; nothing in this section exercises it. Open item, see
> [`quickstart-run-2026-09-22.md`](./quickstart-run-2026-09-22.md) §2. That last distinction is the whole point: a
future change whose temporal term floored at zero would otherwise look *maximally recent*.

Then the corrections (FR-067, FR-068, SC-008):

```bash
bin/aisre fixture verify fixtures/vendor-cancellation-01/ fixtures/vendor-reschedule-01/
bin/aisre query history 'vendor.notice=payments-provider/<id>'
```

`history` answers in both dimensions always — there is no flag, because a version table with one
dimension is not what this store is for. Expected: the prior belief is **still recoverable** by an
observed-time query; **zero** valid intervals rewritten; and for the reschedule, **one** change node with two recoverable windows —
never two nodes.

---

## 3. One announcement, two sources, one change

```bash
bin/aisre fixture verify fixtures/vendor-duplicate-two-sources-01/
bin/aisre resolve why 'vendor.notice=<vendor>/<id>' 'vendor.notice=<vendor>/<id2>'
```

Expected: **exactly one** change node (SC-009); `resolve why` shows the claims, the rule, the score
and the rationale — and shows the merge was made by **a published rule, not by the feeder**
(constitution VI). The failure this guards against is two adjacent entries for one maintenance
window in the ranked list, at 02:10, in front of someone being paged.

---

## 4. The two rollouts, and the rollback

```bash
bin/aisre fixture verify fixtures/gcp-rollout-traffic-shift-01/ \
    fixtures/gcp-revision-zero-traffic-01/ fixtures/gcp-rollback-01/
```

Expected:

- **two** ROLLOUT changes for one deploy — the revision creation, marked as **not** having moved
  traffic, and the traffic shift, marked as having (FR-016, FR-017);
- the traffic shift's valid time equal to the instant defined in
  [`contracts/gcp-feeder.md` §3.2](./contracts/gcp-feeder.md) — the `operation.last = true` audit
  completion entry — exactly, in 100% of cases (SC-002);
- a revision created with **0% traffic** present as a node, with its creation change, and **not**
  presented as serving; its creation change **not** reinterpreted when traffic later arrives (FR-018);
- after a rollback, the service's `traffic_split` property has **three** versions in valid time, and
  a query as of the intermediate instant reports the new revision serving.

```bash
bin/aisre query subgraph 'gcp.cloudrun.service=<p>/<r>/<svc>' --as-of <mid-rollout> --hops 1
```

---

## 5. Execute the algebra against a recorded world

No GCP account; the recorded world stands in for one.

```bash
# The terms, cost classes and window caps. Without --gcp this lists the backends REGISTERED with
# the server, which on a fresh one is none; --gcp prints the declaration with no credential.
bin/aisre backend list --gcp <org-slug>

# One term against the recorded world. --mode recorded makes no network call.
bin/aisre worker call gcp:<org-slug> errors_by_version \
    --mode recorded --recording fixtures/gcp-rollout-traffic-shift-01/world \
    --args '{"...": "..."}'
```

Expected from `backend list`: vendor `gcp`, the **eight** telemetry terms and nothing else — a
backend declaring a graph or knowledge term is **rejected at registration** — each with exactly one
published cost class, and `read_only: true` on every row.

Then the three answers that are easy to get wrong:

| ask | expected | why it matters |
|---|---|---|
| `errors_by_version` over the shift | grouped by **Cloud Run revision**, revision values joining the WORKLOAD nodes | "the new revision is erroring and the old one is not" is the single most useful sentence the corpus contains (FR-089) |
| `error_spans` | `NO_DATA`, coverage naming the **trace data source as absent** and stating nothing was searched | the organisation has no tracing; a consumer must not read this as "there were no error spans" (FR-091, SC-011) |
| anything outside the algebra, or carrying caller query text | **refused**, naming the operation asked for **and** the operations available; nothing executed | the backend is not a query proxy (FR-087, SC-015) |

And the containment check, which is a fixture rather than a review item:

```bash
bin/aisre fixture verify fixtures/gcp-telemetry-rejection-01/
```

Expected: the event carrying a metric sample is **REJECTED** with a published reason code. Zero
digests, and zero parts of one, anywhere in the graph (FR-109, SC-013).

---

## 6. Read the report rows

```bash
bin/aisre fixture verify fixtures/gcp-*/ fixtures/vendor-*/ --report
```

| row | requirement | expectation |
|---|---|---|
| miss rate | FR-108 | at or below the published threshold |
| digest parity | FR-106, SC-014 | identical on **every** field including the whole coverage block and every join key |
| coverage vs GCP | FR-144, SC-001 | differences **enumerated**, not summarised |
| budget | SC-022 | usage matches the recording **to the exact call** |
| body containment | SC-010 | zero bodies, addresses, display names, verbatim subjects, attachments |
| canary survival | SC-018 | **zero** |

---

## 7. Point it at GCP — Dana's scenario

**Needs**: the read-only service account (FR-003), and Dana to have read
`docs/connectors/gcp.md` first, because FR-154 says she approves a **number** before it runs.

```bash
bin/aisre feed gcp --projects <p1>,<p2> --regions <r1> --dry-run
```

`--dry-run` performs §2.1's startup check and nothing else. Two possible outcomes, and both are
success:

- **it starts** — the principal holds no write permission in any area it reads, and it names the
  parts it could **not** verify, requiring the operator's assertion in configuration rather than
  assuming (FR-004);
- **it refuses** — naming **exactly which permissions offend**. A project-owner service account must
  produce this. If it starts, the check is broken, and that is the first thing to test.

Then, for real:

```bash
bin/aisre feed gcp --projects <p1> --regions <r1> --record fixtures-private/campaign-<date>/
bin/aisre feed vendor-notice --allowlist config/vendors.yaml --record fixtures-private/campaign-<date>/
```

Note the **two separate commands with two separate credentials** (FR-002). They share no credential,
no cadence, no budget and no checkpoint; a mailbox outage must not look like a topology gap.

> **Start recording before the scope is settled.** FR-130 is not advice. Topology history and an
> announcement stream **cannot be reconstructed retrospectively**, and the scope in force is
> recorded in every checkpoint precisely so that starting early costs nothing later.

---

## 8. Sanitise, scan, sign, commit — Casey's scenario

**Needs**: the campaign from §7, the corpus HMAC key, and a named signatory.

> **`fixture campaign record|sanitise|scan|sign` are implemented** (T153–T155, 2026-09-22). The
> command sequence below is the real one, and `record` comes first — the gates read the campaign
> record, so a recording with none cannot be gated at all. `parity` (T159) is not registered: it
> compares a live run against the recording of the same run, and there is no live side until the
> poller and a credential exist.
>
> Two things the names promise more than they deliver, both stated by the commands themselves:
> `sanitise` is **not the first cleaning** — FR-137 puts that in the connector before anything
> touches disk, so this asserts the recording is clean and stamps the policy version — and `scan`
> **does not satisfy FR-138**, because this binary contains the sanitiser and a check run from inside
> it shares every defect the sanitiser has. `scripts/check-no-secrets.sh` is the independent one.

```bash
# First, before recording: the campaign's own account of itself (FR-130, FR-131, FR-132).
bin/aisre fixture campaign record fixtures-private/campaign-<date>/ \
    --org <slug> --started-at <when recording began> \
    --projects <p1> \
    --mailbox-path shared_mailbox --authorised-by <name> --authorised-on <date> \
    --signatory <name> [--signatory <second name>]

# When the scope changes mid-campaign — which FR-130 expects, since recording starts first.
bin/aisre fixture campaign record fixtures-private/campaign-<date>/ \
    --add-scope <instant> --projects <p1>,<p2> --scope-why '<what changed>'

# Then the gates, in this order.
bin/aisre fixture campaign sanitise fixtures-private/campaign-<date>/
bin/aisre fixture campaign scan     fixtures-private/campaign-<date>/
bin/aisre fixture campaign sign     fixtures-private/campaign-<date>/ --signer <name>
./scripts/check-no-secrets.sh                  # FR-138's independent scan; the one above is not it
```

Expected, and each is a **hard** failure rather than a warning:

| gate | expectation |
|---|---|
| canaries | **zero survive**; a survivor **fails the commit** (FR-139, SC-018) |
| people identifiers | zero in any committed artifact **in any form, hashed included** (FR-135, SC-019) |
| unsanitised payloads on disk | zero, at any point, **including from the aborted run** — leave one deliberately and re-run the scan (FR-137) |
| manifest | names the policy **version**, the content hash, the signatory, the date and **what was dropped**; a second signature where a second person exists (FR-140) |

Sanitisation already ran **in the connector before anything touched disk** during §7 (FR-137). The
`sanitise` step here is the corpus-level pass and the place the policy version is stamped — it is
not the first time the data is cleaned, and an implementation where it is the first time is wrong.

A payload that cannot be made safe is **dropped and the drop documented** (FR-140). Never committed
on the promise of being cleaned later.

---

## 9. Live parity

**Needs**: §7 and a window whose data has settled.

```bash
bin/aisre fixture campaign parity <live-recording>/ --recorded fixtures-private/campaign-<date>/
```

Both sides are recordings, because a live run's output **is** one: `feed … --record` writes it. So
the command is runnable today against any two recordings — which is how it is tested — and the run
FR-143 actually asks for needs the live poller and a credential.

Two assertions (FR-143, SC-016, SC-014):

1. the graph built **live** equals the graph built from the **recording of the same run**, byte for
   byte, as in feature 001's live-run parity check;
2. a digest computed **live** over a settled window equals the digest computed from the **recorded
   response** for the same request — on every field including the whole coverage block and every
   join key, not only the summary statistics.

The second is the one that catches a backend deriving anything from the connection rather than from
the response or from configuration (FR-008).

---

## 10. Re-run the coverage audit — the feature's actual gate

**Needs**: the thirteen-incident corpus and the published audit method.

```bash
bin/aisre audit coverage --input docs/evaluation/coverage-audit-2026-09.json \
    --feeder-set otel+k8s+gcp+vendor-notice \
    --out docs/evaluation/
```

**Expected: the true cause was a node or change in the graph at alert time for at least 8 of the 13
incidents** — the ADR-0004 figure (SC-023). And the output must carry, per incident:

- whether the cause was in the graph at alert time;
- **which feeder** would have supplied it;
- where it was not, whether the correct engine answer is `unobserved` or `not_change_induced`, with
  its reason.

Then the demonstration, rather than the assertion (SC-024):

```bash
bin/aisre investigate replay --from fixtures-private/incidents/<missed-notice-incident>/
bin/aisre investigate get <id> --chain      # the evidence chain in the order it was produced
```

Expected: for at least one recorded incident whose true cause **was** a missed vendor notice, the
notice's change node appears **in the top three** of the ranked change list at the alert's reference
instant. That is the pattern this feature exists to defeat, shown working.

And finally Sam's scenario, which is the one that decides whether any of this is usable at 02:10
(SC-025):

```bash
time bin/aisre investigate 'gcp.monitoring.alert_policy=<policy-id>' --at <alert-instant>
```

Expected in **one command and under thirty seconds** on the recorded corpus: the alert, what it
watches, what changed around it in the preceding window **including any announced vendor window that
intersects it**, who owns it, and executable pointers.

---

## 11. If a fixture goes red after Google changes an answer

By design. FR-008 and the edge cases require that **a payload that no longer parses fails loudly
against its fixture** rather than silently producing fewer events, and that the integration reports
the rejection **with the field that broke**.

```bash
bin/aisre fixture verify fixtures/gcp-baseline-topology-01/ --log-level debug
```

The diff names the field. That is the intended experience, and Casey asked for exactly it: *"wants
to know, when Google changes an answer, which fixture goes red."* Fewer events and a green suite
would be the bug.
