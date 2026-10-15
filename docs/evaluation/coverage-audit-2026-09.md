# Coverage audit, September 2026 (aggregate)

Per ADR-0003 D1 and spec 002 User Story 0: before tuning the reasoning layer, measure the
fraction of real incidents whose true cause would have been a node or change in the graph at
alert time. This is the aggregate result for the first organisation audited (the owner's).
Incident-level detail is kept in a private corpus, not in this repository.

**Corpus**: 13 classifiable production incidents over nine months, drawn from the organisation's
incident channels (its incident-management product is unused). Two security incidents excluded,
one incident unclassifiable.

| Feeder set | Cause is a node or change at alert time | Symptom visible, cause external | Not change-induced or unobservable |
|---|---|---|---|
| Feature 001 feeders only (OpenTelemetry spans, Kubernetes) | 0 % | 15 % | 85 % |
| + deploy feeders for the actual platforms (serverless revisions, CI/CD, frontend hosting) | 23 % | 15 % | 62 % |
| + vendor change feeders (status pages, maintenance and deprecation notices, API changelogs) | 62 % | 15 % | 23 % |

**Ceiling**: about 62 % with the best realistic feeder set. Two further incidents (15 %) are
symptom-visible but cause-unobservable: a vendor quota behaviour and a latent race surfacing as
delivery errors. The unobservable remainder (23 %) is client-side configuration, a business
data change and a credential leak, for which the correct engine answer is `not_change_induced`
or `unobserved`; with the latent bug, these are the four categories that owe the corpus fixtures.

**Vocabulary note (2026-09-17)**: the cause vocabulary was extended *after* this audit was
transcribed. The twelve values FR-070 published had no member for a deployment, a configuration
change, a cloud maintenance window, a vendor API change or deprecation, a capacity or quota limit,
or an unplanned provider infrastructure incident — so **six of these thirteen incidents were filed
as `other`**: all three the deploy feeders turn into `observed`, and three of the five the
vendor-notice feeders do. Migration `0007_audit_categories.sql` adds those six values, each
documented with an example class in [`coverage-audit-format.md`](coverage-audit-format.md). The
figures above are unaffected, because the ceiling, π₀ and the ladder count verdicts and not
categories. The private transcription was redone the same day with the eighteen-value set and
this aggregate regenerated from it: no incident is filed as `other` any more. The six now read as
two deployments, one configuration change, one cloud maintenance, and two vendor API changes,
which is the breakdown the feeder order in ADR-0004 rests on.

**Consequences adopted**:

1. The next feeders outrank the reasoning layer for this organisation, in this order: the cloud
   platform's deploy/revision events and its logging and monitoring as a telemetry backend; CI/CD
   and frontend-hosting deploys; a vendor-notice feeder (missed maintenance and deprecation
   emails were the single most repeated cause).
2. The OpenTelemetry topology feeder has no input in this organisation (no tracing anywhere);
   the Kubernetes feeder covers one inference cluster.
3. Alert intake must accept human-declared incidents: most incidents were noticed by people,
   and the incident channel is the real trigger. Monitor transitions caught few of them.
4. The Datadog integration's value here is log search for one service and a handful of
   monitors; its service-map story does not apply to this organisation and should not be the
   first vendor connector built.

The categories of the 23 % should each become ground truth for `unobserved` /
`not_change_induced` fixtures (spec 002 FR-061b, SC-023).

**Machine-readable form**: [`coverage-audit-2026-09.json`](coverage-audit-2026-09.json), in the
shape `aisre audit coverage` emits (FR-069a). It is what the ceiling guard, `audit coverage
compare` and the ledger's π₀ read; every figure above is asserted against it by
`internal/investigation/audit`, so the two cannot drift. The input format and how a private audit
is transcribed into it are documented in
[`coverage-audit-format.md`](coverage-audit-format.md).

## Knowledge re-run (007 T031, 008 T043), 2026-10-15

The audit was re-run with the knowledge index populated from the first recording of the owner's
postmortem database (seven postmortems, recorded 2026-10-15, kept in the private corpus).

- **The thirteen incidents: 0 citations, correctly.** None of them has a postmortem in the
  database: six postmortems describe incidents after the audit window and one an incident outside
  its channel set, and every page was written on 2026-10-15. The as-of rule allows no citation, and
  none is made. The attribution above is unchanged: a postmortem is retrieved knowledge, not a
  change node, so it never moves an incident between verdicts.
- **SC-002 on the seven postmortems: met.** Each was queried with a symptom taken from past the
  first 600 characters of its summary properties. 008 retrieves all seven at rank 1, lexical and
  hybrid. 007's summary match finds 4 of 7 when scoped to the postmortem's service and 3 of 7
  when no linked entity is named.
- **One precedent inside the database.** Two incidents on the same node the same day: with only
  the earlier postmortems indexed, the later incident's symptoms rank the earlier postmortem 2nd,
  behind another relevant one. It could not have been cited at the time, because it was written
  afterwards.

Seven documents is a small corpus: these figures say the sections are indexed and reachable, not
how ranking holds over hundreds of pages.
