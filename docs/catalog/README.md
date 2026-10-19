# The service catalog

The agent keeps its own list of services, and decides which one each signal is about: an alert, a
declared incident, a postmortem or runbook, a change. No connector is the source of truth, because a
deployment may not run any particular vendor. A person can correct any decision, and the correction
wins for good. Feature 009 ([spec](../../specs/009-service-catalog/spec.md),
[ADR-0015](../decisions/ADR-0015-shared-contract-additions-for-009.md)).

## Entries and aliases

An **entry** is a service or a platform, named by a slug (`candidate-view`) that never changes, with a
display name, a kind, an owner, a description and a status: `proposed` (discovered, not yet
confirmed), `confirmed` or `hidden`. It lives in the graph as a `SERVICE` node in the `sre.catalog`
namespace, so it replays and reads as of any instant like everything else.

An **alias** says that a name a connector uses denotes an entry: `datadog.service=production/candidate-api`,
`notion.value=<database>/Candidate View`, `slack.slug=checkout`, optionally for one environment.

An alias is **not a merge**. The production and staging `candidate-api` stay two entities, each with
its own history, and both are aliased to *Candidate View*. Merging them would put staging's changes in
every production investigation. A hidden name (`aisre catalog hide --name ns=value`) says that a name
is not a service at all.

```sh
aisre catalog list                     # entries and their aliases, now or --as-of
aisre catalog show candidate-view      # one entry, with the decision behind each alias
aisre catalog import catalog.yaml      # keep the catalog in a versioned file, if you like
aisre catalog alias add datadog.service=production/candidate-api candidate-view
```

## Discovery

You do not have to write the catalog first. The attributor proposes an entry for every `SERVICE`
entity no alias names, and for every Notion value it has not seen. When the slug is already taken, it
proposes an alias to the existing entry instead. A proposal is raised once. Accepting one is declaring
the alias; rejecting it is durable.

```sh
aisre catalog proposals
aisre catalog alias accept "notion.value=<db>/Candidate View" candidate-view
aisre catalog alias reject datadog.service=staging/candidate-api candidate-api
```

## The decision model, `attribution/rules@1`

Layers in order; the first that decides wins.

| layer | rule | decides when | score |
|---|---|---|---|
| 0 correction | `H` | a person said what this signal, or one of its names, is about | 1.0 |
| 1 alias | `A1` | one of its names is an alias; a person's alias outranks a discovered one | 1.0 |
| 2 probable | `P1`–`P4` | equal names, shared words, topology, shared owner, combined by noisy-OR | 0.50–0.99 |
| 3 classifier | `M` | optional; reads the signal's text against the entries' descriptions | calibrated |

Layers 2 and 3 attribute only at a score of at least τ = 0.80, leading the next entry by δ = 0.10.
Anything else is **`unknown`**, with up to three ranked suggestions. The model never guesses: a wrong
attribution scopes an investigation to the wrong service, an `unknown` asks a person.

Every decision records its layer, rule, score, rationale, evidence and ruleset, and `aisre attribute
why <signal>` prints them, with the history.

## Corrections

```sh
aisre attribute correct datadog.monitor=4242 --entry infra                     # this signal
aisre attribute correct datadog.monitor=4242 --entry infra --name datadog.service=production/edge   # and every signal named so
aisre attribute revert <correction event id>
```

A correction is a human decision with its principal and instant. A name-level correction also becomes
an alias, so the next signal carrying the name is attributed without anyone asking. An investigation
pinned before a correction replays exactly as it ran.

## The classifier, and plugging in another

Layer 3 is off unless the model configuration has an `attribution.classifier` block. The shipped one
embeds the signal's text and each entry's profile (name, description, aliases) through the same
embedding model as knowledge retrieval, hosted or self-hosted, and turns the cosine into a probability
with a logistic calibration fitted for that model:

```yaml
attribution:
  ruleset: attribution/rules@1
  classifier:
    kind: embedding
    timeout: 2s
    calibration: {slope: 72.4, intercept: -53.6}   # fitted for mistral-embed on fixtures/attribution/classifier-01
```

It is asked only about a signal layers 0–2 left `unknown`. An error, a timeout or a missing credential
leaves the decision to layers 0–2, and the evidence says why: the classifier never blocks a signal.

Any classifier can take its place: implement `pkg/attribution.Classifier` (two methods, `ID` and
`Classify`), prove it with `pkg/attribution/classifiertest.Run`, and pass it with
`catalog.WithClassifier`. A language model, a local scorer or a remote service all fit; the contract
is calibrated scores in [0, 1], deterministic on the same input, only for the entries given.

## Measuring it

```sh
aisre eval attribution fixtures/attribution/
```

replays every labelled case, runs the attributor, and reports precision, coverage and ten-bin
calibration per layer. CI fails when the precision of the attributor's own inferences falls below
0.95, or a populated bin is more than 10 points from its observed accuracy. A signal a person
corrected is scored on the decision the correction overruled. Attributions that follow an alias a
person declared are reported and not gated: when they are wrong, the catalog is.

`aisre feed notion --check` and `aisre feed slack --check` list every name the connector sees with the
entry it maps to, or `unmapped`, or `hidden`.

## Moving off the connectors' old maps

007's Notion `entities` map and 006's Slack `--service` flags still work, and are deprecated. Move them
once:

```sh
aisre catalog import --from-notion-config notion.yaml --dry-run
aisre catalog import --from-slack-flags checkout=service.name=checkout --dry-run
```
