# The coverage-audit input format

The input to `aisre audit coverage` — a list of past incidents with their alert instants and
their known causes (spec 002 User Story 0, FR-069). This document is the format's contract and
the instructions for transcribing a private audit into it.

The audit **measures; it never infers**. Every cause in the list is supplied by a human who
looked at a graph running a named feeder set and answered one question per incident: at the
instant the alert fired, with observed time pinned to it, would the true cause have been a node
or a change in the graph? Nothing in this pipeline derives a cause, and a list that supplies none
is refused rather than guessed at.

## The one property that shapes everything else

**There is no free-text field anywhere in this format.** Every value is an identifier the owner
chooses, a date, or a member of a closed set. No title, no description, no "notes", no
free-text cause.

That is not tidiness; it is what makes the format usable. The audit it exists to carry is a
transcription of real production incidents, and its detail must stay out of this repository
(FR-069a). A "notes" column is how a customer name, a hostname, a ticket title or a stack trace
ends up published. So the fields were chosen as the smallest set that computes a ceiling: a
category, a class, and a verdict per feeder set. The `stated_cause` the database records is
composed from two of them — `third_party_outage/change_induced` — precisely because there is no
prose to record.

## Shape

YAML or JSON; both are read by the same strict decoder, and **unknown keys are rejected**. A
misspelled field in an audit input is a silently wrong ceiling, and a wrong ceiling is a wrong
π₀ in every investigation that cites it.

```yaml
version: 1                       # ListVersion; this build reads 1
audit_id: 2026-09                # stable, cited by every target and every ledger row
run_at: 2026-09-17               # a date, or an RFC 3339 instant
author: <issuer>|<subject>       # the principal who did the work; a FK into graph.principals

corpus:
  label: owner-org               # names the corpus, so two runs can be shown to be over one
  from: 2025-12-01
  to: 2026-08-31
  excluded:                      # incidents of the period NOT audited, with coded reasons
    - {ref: INC-0041, reason: security_incident}
    - {ref: INC-0055, reason: unclassifiable}

feeder_sets:                     # cumulative: set n+1 is set n plus the feeders it names
  - {name: feature-001,    order: 1, feeders: [otel.spans, kubernetes.workloads]}
  - {name: +deploy,        order: 2, feeders: [serverless.revisions, cicd.deploys]}
  - {name: +vendor-notice, order: 3, feeders: [vendor.status_pages, vendor.api_changelogs]}

incidents:
  - ref: INC-0007                # opaque; NEVER a title
    alert_at: 2026-01-27         # a date is enough, and leaks less than an instant
    cause:
      category: third_party_outage   # one of the published eighteen
      class: change_induced          # change_induced | not_change_induced | unobserved
      ref: null                      # OPTIONAL graph id, for checking a verdict by hand
    observability:               # one verdict per declared feeder set; all of them required
      feature-001: not_observable
      +deploy: not_observable
      +vendor-notice: observed
```

### `cause.category` — the published eighteen

| category | what it is | example class of incident |
|---|---|---|
| `flag_flip` | a feature flag turned on or off | a flag rolled to 100 % of traffic |
| `iac_apply` | an infrastructure-as-code apply | a Terraform apply that rewrote a load-balancer rule |
| `db_migration` | a database migration | a schema change that locked a hot table |
| `certificate_expiry` | a certificate or key that expired | a TLS certificate nobody renewed |
| `scheduled_job` | a cron or scheduled job | a nightly batch that ran long, or ran twice |
| `third_party_outage` | a SaaS or upstream vendor outage | a payment or auth provider returning 5xx during its own incident |
| `traffic_shift` | a change in traffic shape or volume | a marketing send, a retry storm, a customer's bulk import |
| `latent_bug` | a defect shipped earlier and triggered later | a race condition that only surfaces above some concurrency |
| `client_side_configuration` | configuration held outside the system under audit | a customer's firewall rule, proxy or pinned SDK version |
| `business_data_change` | a data change made through the product itself | an entitlement or plan edited in the product's own admin UI |
| `credential_leak` | a leaked or revoked credential | a token revoked after exposure, or a rotated key not redeployed |
| `deployment` | a release of application code to an environment | a service revision rolled out, or rolled back |
| `configuration_change` | a change to configuration the audited system owns, with no code release | an environment variable, an autoscaling floor, a routing rule |
| `cloud_maintenance` | planned or announced work by a cloud or infrastructure provider | a managed-database failover window, a node-pool upgrade |
| `vendor_api_change` | a vendor changed or deprecated an API, a schema, a default or a quota policy | a deprecated endpoint switched off, a response field removed |
| `capacity_limit` | a quota, rate limit, pool or capacity ceiling reached or moved | a vendor quota lowered, a connection pool exhausted, a disk filled |
| `infrastructure_incident` | an unplanned provider network or hardware incident | a zone network partition, a host failure, a managed service degraded |
| `other` | a stated cause that fits none of the above | rare by design: a cause filed here is a prompt to ask what member the vocabulary is missing |

Three distinctions do work, so they are worth stating rather than guessing at:

- `configuration_change` is configuration **inside** the audited system — a feeder could carry
  it. `client_side_configuration` is configuration outside it, which no feeder of this system
  ever sees; that is why one is ordinarily change-induced and the other is not.
- `cloud_maintenance` is **planned** provider work, announced somewhere a maintenance-notice
  feeder can read. `infrastructure_incident` is the unplanned version of the same thing.
- `infrastructure_incident` is kept apart from `third_party_outage` because the two rank
  different feeders (FR-070): the first is read from the cloud provider's health feed, the
  second from that vendor's status page. If they were one value the audit could not say which
  feeder to write next, which is the whole job of the category breakdown.

These are exactly the values `investigation.coverage_audit_items.category` accepts — migration
`0006_investigation.sql`, widened by `0007_audit_categories.sql` — and a test parses the
highest-numbered migration that defines the constraint and asserts the two lists are identical in
content and order.

**The set was twelve until 2026-09-17.** FR-070's original list was the causes the feature-001
feeders miss, and it had no member for the most ordinary change-induced causes of all: a
deployment, a configuration change, a provider maintenance window, a vendor API change, a
capacity limit. Transcribing the September 2026 audit put six of thirteen incidents under
`other`. `0007_audit_categories.sql` adds the six above, additively: nothing was renamed and
nothing was removed, so an audit transcribed under the twelve still loads, still validates and
still means what it meant — it is simply coarser than one transcribed today.

### `cause.class` — what a corpus fixture would have to assert

| class | meaning | corpus obligation |
|---|---|---|
| `change_induced` | a change caused it | none |
| `not_change_induced` | no change caused it | a fixture whose ground truth is `not_change_induced` with this category (FR-071b) |
| `unobserved` | a change or an act caused it, but no feeder could carry it | a fixture whose ground truth is `unobserved` with this category (FR-071b) |

### `observability` — the verdict per feeder set

| verdict | meaning | counted as |
|---|---|---|
| `observed` | the cause itself would have been in the graph at alert time | the ceiling's numerator; `cause_present` |
| `symptom_only` | the effect was visible, the cause was not | `cause_absent`, reported in its own column |
| `not_observable` | no feeder in the set could have carried the cause | `cause_absent`, and part of the unobservable remainder |
| `undecidable` | the auditor could not decide | `undecidable` — leaves the ceiling's **denominator** |

`symptom_only` is deliberately *not* folded into the ceiling. The effect being visible is not the
cause being present; counting it would publish a recall bound no engine could reach. It is
reported separately because the distinction tells you whether the next feeder adds localisation
or adds causes.

An `undecidable` verdict is written as a mapping and **must** carry a coded reason —
`cause_never_established`, `conflicting_accounts`, `outside_retention`, `no_record`,
`feeder_set_untested`:

```yaml
    observability:
      feature-001: {verdict: undecidable, reason: outside_retention}
```

"Undecidable" with no reason is indistinguishable from "nobody looked", so the format refuses it.

### Exclusion reasons

`security_incident`, `unclassifiable`, `duplicate`, `not_production`, `outside_window`.

The ceiling is meaningless without its denominator, and the denominator is meaningless without
the exclusions that produced it, so they are part of the input and part of the published result.

## What the parser refuses

Each refusal is a refusal to guess:

- an incident with no `cause.category` or no `cause.class` — *the audit measures, it never
  infers* (FR-069);
- a value outside any of the closed sets above;
- an incident that says nothing about a declared feeder set, or names one that was not declared;
- an `undecidable` verdict with no reason, or a reason on any other verdict;
- observability that **regresses** as feeders are added — feeder sets are cumulative, so
  adding feeders can only move an incident up the scale `not_observable < symptom_only <
  observed`. A regression is a transcription error, and it would surface later as a ceiling that
  moved for a reason nobody could attribute (FR-071a);
- an unknown key, a duplicated incident reference, an incident that is also in `excluded`, a
  version this build does not read, or an audit with no author.

## Commands

```sh
# Classify the list and publish the ceiling. --db and --out are both optional.
aisre audit coverage --input <file> [--db <DSN>] [--out <dir>] \
                         [--feeder-set <name>] [--aggregate-only] [--output table|json]

# Compare two runs and attribute the movement to the feeders added (FR-071a).
aisre audit coverage compare <before.json> <after.json>

# Fail the build when a ceiling-bounded target exceeds the ceiling (FR-071). Exits 4.
aisre audit coverage guard --report <eval-report.json> --audit <audit.json>

# Name the remainder categories the corpus does not cover (FR-071b). Warns; exits 0.
aisre audit coverage gaps --audit <audit.json> [--fixtures fixtures/incidents]
```

`--incidents` is an alias of `--input` and `--graph-config` an alias of `--feeder-set`, both
spelled as `specs/002-investigation-engine/contracts/cli.md` publishes them. Setting an alias and
its primary to different values is refused rather than silently resolved.

`--out <dir>` writes two files: `coverage-audit-<id>.json`, the machine-readable result every
downstream consumer reads, and `coverage-audit-<id>.md`, the aggregate rendering that gets
checked in. `--db` records the run in `investigation.coverage_audits` and
`coverage_audit_items`, idempotently — re-running rewrites the same rows rather than
accumulating a second copy.

## What the audit produces

- **Per feeder set**: observed / symptom-only / unobservable / undecidable counts, the ceiling
  (`observed / classifiable`), π₀, the category breakdown, the missing causes ranked by count —
  which is the ranking of the feeders worth writing next (FR-070) — and the unobservable
  remainder.
- **Overall**: the ceiling of the feeder set in force, **with the incident count it rests on**,
  π₀ = 1 − ceiling, the corpus and its exclusions, and the input digest.

π₀ is read by the hypothesis ledger through `audit.PriorFromAudit` and recorded on every
investigation together with the audit id it came from, so a historical confidence stays
interpretable (ADR-0005 D9, FR-019a).

## Transcribing a private audit without leaking it

The detailed audit lives in the owner's private notes. This is how it becomes a file that can be
published, and why nothing private travels with it.

1. **Keep the input outside this repository.** Write it wherever the private corpus lives — the
   audit needs no database and no server, so `aisre audit coverage --input ~/private/audit/incidents.yaml`
   works from anywhere. The repository never has to see it.
2. **Choose opaque references.** `INC-0007`, or the incident-tracker id. Never a channel name, a
   title, a service name or a customer. The reference reaches
   `coverage_audit_items.incident_ref` and nothing else does.
3. **Prefer date-only `alert_at`.** The audit's arithmetic needs no minutes, and a date is a much
   weaker join key against a private incident record than a timestamp to the second. Both
   spellings are accepted; the date is the one to use.
4. **Reduce each cause to a category and a class.** This is the transcription step, and it is the
   step that does the work: "the vendor's OAuth endpoint was down for 40 minutes during the
   Tuesday sale" becomes `{category: third_party_outage, class: change_induced}`. Everything that
   could identify anybody is dropped at this line, by hand, before anything is written down.
5. **Record the verdict per feeder set, not an explanation of it.** The reasoning stays in the
   private notes; the file carries `observed`, `symptom_only` or `not_observable`.
6. **Leave `cause.ref` empty unless the identifier is already public.** It is optional and exists
   only so a reviewer can re-check a verdict against a graph they already have.
7. **Publish with `--aggregate-only`.** The result then carries no per-incident rows at all, and
   both the JSON and the Markdown are safe to check in. A test asserts that an aggregate contains
   no incident reference.
8. **If the list itself must be shared**, it already can be: with steps 2 to 6 followed, every
   field in it is an opaque id, a date, or a member of a published closed set. That is the
   property the format was designed for — there is nothing in it to redact, because there was
   never anywhere to write anything that would need redacting.

The synthetic fixtures under `fixtures/audits/` are worked examples of the finished shape. They
are invented end to end and reproduce the *shape* of the published September 2026 aggregate —
13 incidents, 0 / 3 / 8 observed across three feeder sets, a ceiling of `8/13 = 0.615385` and
π₀ of `0.384615` — so the test suite can assert a real ceiling without any real data.
