# Feature Specification: Deploy Feeders — GitHub and Vercel

**Feature Branch**: `004-deploy-feeders`

**Created**: 2026-09-17

**Status**: Draft

**Input**: User description: "Two thin deploy feeders — GitHub (Actions workflow runs, the
deployments API, releases) and Vercel (deployments, promotions, rollbacks, environment
variables) — sharing one mapping: a deployment is a change node. Each emits ROLLOUT change nodes
with an actor kind, an origin link, a valid time taken from deployment completion, and identity
claims on the commit, the artefact and the release so that the same rollout observed by the
platform (Cloud Run in feature 003, Kubernetes in feature 001) resolves to one change node and
not two. Two transports per platform: a signature-verified webhook that is only a doorbell, and
polling that is the truth. Read-only credentials; a credential with write scope is refused."

## Why this feature exists

The coverage audit (`docs/evaluation/coverage-audit-2026-09.md`) measured the ceiling before the
reasoning layer was tuned, and the number that matters here is the first row: with the feature
001 feeders alone — OpenTelemetry spans and Kubernetes — the true cause of **0 of 13** production
incidents would have been a node or a change in the graph at alert time. With deploy feeders for
the platforms the organisation actually ships on, it is **3 of 13** (23 %). Two of those three
incidents were shipped by **GitHub Actions** and **Vercel**; the third was a Cloud Run revision
and belongs to feature 003. ADR-0004 D2 turns that finding into this feature.

Three things become true that are not true today:

1. **"What changed?" has an answer for the platforms in use.** The organisation runs core
   applications on Cloud Run and Vercel and ships them from GitHub Actions. Until a feeder reads
   those, a deployment-caused incident has no candidate change in the graph at all, and the
   ranker has nothing to rank — it is not wrong, it is empty.
2. **A rollout is one event, not one per observer.** The same rollout is visible to the pipeline
   that started it (GitHub), to the hosting platform that ran it (Vercel, Cloud Run, Kubernetes),
   and sometimes to an observability vendor. Every one of those is telling the truth. If the
   graph shows a rollout three times, the ranked change list is three near-identical entries and
   the on-call SRE stops trusting it. This feature is where the claim keys that let the published
   certain rules collapse them into one change node are minted.
3. **The change carries who did it.** ADR-0003 D2 ranks against symptom onset and types
   system-originated changes apart from human ones. A deploy feeder is the natural place for that
   distinction to originate: a push by a person, a scheduled workflow, a bot re-running a job and
   an automatic promotion are four different things, and the platform states which is which.

This is deliberately **two thin feeders in one feature**, not two features, because they share
one mapping — a deployment is a change node with a target, an actor kind, an origin link and a
set of claims — and because the value of either alone is halved: the organisation's front end
deploys on Vercel and its services deploy from GitHub Actions, and most incidents in the corpus
touched one of each. Neither feeder implements a telemetry backend: they write pointers, and
executing a pointer is feature 003's and feature 005's job.

## Clarifications

### Session 2026-09-17 (c)

Remediation of the `/speckit-analyze` cross-artifact pass over features 002–005. Findings closed
here: **X2** (the actor-kind set) and **I12** (enum names in normative text). **X3** (the
`alert.transition` idempotency key) touches nothing in this specification: these feeders emit
rollout and configuration changes, not alert transitions, and FR-020's rollout key — (source,
origin run or deployment identifier, attempt, target) — is a different key for a different event
and is unchanged.

- Q: What is the published actor-kind set, and where does "system-originated" land in it? (X2, I12
  — FR-013, FR-027, FR-037, Key Entities "Actor kind", "Dependencies on feature 001") → A:
  `{PERSON, AUTOMATION, CONTROLLER, VENDOR, UNKNOWN}` (ADR-0005 D1), written with the enum name and
  the prose gloss in parentheses. What this specification called "system-originated" splits
  normatively: **CI runs and bot accounts are `AUTOMATION`** (machinery acting on a person's behalf,
  still a candidate cause), while **autoscalers and schedulers are `CONTROLLER`** (machinery
  reacting to state, which feature 002's causal ordering may exonerate after onset). Neither is ever
  derived from the actor's name; where the platform states nothing, the kind is `UNKNOWN` with the
  evidence recorded.

### Session 2026-09-22

Closes the two open questions this specification carried into planning: the read scope (FR-008) and
the monorepo mapping (FR-017).

- Q: Which repositories and which Vercel projects are in scope for v1 and for the first recording
  campaign — every repository in the organisation that has a deployment, a release or a deploy
  workflow, or a named allowlist agreed with the platform owner? (FR-008) → A: **Neither: the
  credential's own grant is the scope.** This is a GitHub App the owner installs and chooses
  repositories for, so the App installation's repository selection *is* the allowlist, and the
  feeder enumerates it from the platform at startup instead of keeping a second list beside it.
  Two lists cannot disagree if there is only one, and the operator changes scope where they already
  expect to — in the installation — rather than in a connector file that has to be kept in step
  with it. It also keeps FR-003's posture whole: the feeder already asks the platform what its
  credential may do, and now asks the same question about *what* it may read.
  Where a platform has no per-target grant to enumerate, the targets are configuration **within**
  what the credential grants, and the feeder states which of the two regimes it is running under —
  so "not in scope" is never ambiguous between "not granted" and "not configured". Which regime
  Vercel falls under is a Phase 0 research question, not a decision: it depends on what a Vercel
  token can actually be scoped to, and the answer must come from the platform's documentation
  rather than from this specification's preference.

- Q: Where does the repository↔service mapping for a monorepo come from — connector configuration
  owned centrally, or a manifest committed in the repository so that it versions with the code it
  describes? (FR-017) → A: **Connector configuration, owned by the operator.** The versioning
  argument for a committed manifest is real and it loses to two things. It would make the feeder
  read repository **file contents**, widening the credential and adding a new class of input to
  sanitise; and it would make the mapping changeable by anyone who can merge, so what the graph
  believes about which service a deploy touched would be attacker-controllable by the same people
  whose changes the graph exists to attribute. A mapping that can be edited by the change under
  investigation is not evidence.

## Personas

- **Sam, on-call SRE.** Is paged at 14:32. Wants the ranked change list to contain the 14:20
  deploy, once, with who shipped it, what commit it carried, whether it was a rollback, and a
  link that opens the run. Today Sam reads it from a Slack notification if someone posted one.
- **Casey, connector author.** Owns both feeders. Wants the two platforms' shapes to land on one
  published mapping with no special cases in the graph, and wants to know which fixture goes red
  when GitHub changes an answer.
- **Dana, platform owner.** Provisions the GitHub App and the Vercel token. Will not install
  anything that can dispatch a workflow, create a deployment, promote a build or read a secret
  value. Owns the organisation's API rate limits and wants to know the cost before it runs.
- **The investigation engine (machine consumer, feature 002).** Asks for the changes in a window
  around an alert, with actor kinds and hop distances, and expects each real-world rollout to
  appear exactly once with an evidence chain it can cite.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A GitHub deployment is a change, and it is the same change the platform saw (Priority: P1)

The organisation ships services from GitHub Actions. A workflow run finishes at 14:20 and a
deployment to the `production` environment reaches `success`. The feeder turns that into one
ROLLOUT change node per deployed target, valid at the instant the deployment completed, with the
triggering user as the actor and an actor kind that says whether that was a person or CI, a link
back to the run, and identity claims naming the commit, the artefact and the release. When the
Cloud Run feeder (003) or the Kubernetes feeder (001) reports the same rollout, the published
certain rules merge the two observations into **one** change node.

**Why this priority**: This is the feature. It is the half of the audit's 23 % that GitHub
shipped, and it is the story that proves the "one rollout, not two" property on which every other
deploy source depends.

**Independent Test**: Replay the recorded workflow-run, deployment and `deployment_status`
payloads for an incident window from an empty graph and compare the change nodes, their targets,
actors, actor kinds, valid times and claims to the golden output; then replay the same window
together with the platform feeder's recording of the same rollout and check the ranked change
list contains it once.

**Acceptance Scenarios**:

1. **Given** a recorded workflow run that produced a GitHub deployment whose status reached
   `success` at 14:20:03, **When** the feeder runs from an empty graph, **Then** a ROLLOUT change
   node exists with valid time 14:20:03 — the completion instant, not the run's start, not the
   poll instant — carrying the triggering user as its actor, an actor kind, the run URL as its
   origin reference, and a `changed-by` edge to every target it resolves.
2. **Given** the same deployment, **When** its claims are inspected, **Then** it carries
   `deploy.commit_sha` for the commit it shipped, and `deploy.image` and `deploy.release` for
   every artefact identifier and release tag the run names — and the change's target carries a
   `github.repo` claim naming the repository.
3. **Given** the same rollout also observed by the Cloud Run feeder (003) or the Kubernetes
   feeder (001), **When** both sources have been ingested, **Then** the published certain rules
   merge the two change observations into one change node, the audit query names the rule, the
   score and the supporting claims, and the ranked change list for the affected service shows the
   rollout **once**.
4. **Given** a workflow run triggered by a person pushing to the default branch, and a second
   triggered by a schedule or by a bot account, **When** both are ingested, **Then** the first
   carries actor kind `PERSON` and the second `CONTROLLER` for the schedule or `AUTOMATION` for the
   bot account, and neither actor kind is derived from the actor's name alone.
5. **Given** a deployment whose target cannot be resolved to any entity the graph knows, **When**
   the feeder runs, **Then** the change is kept and marked unattached with the unresolvable
   identifiers recorded, and it attaches automatically if the target later appears.
6. **Given** a workflow run that is a test or lint run and produced no deployment and no release,
   **When** the feeder runs, **Then** no change node is created, and the filter that excluded it
   is stated in the checkpoint rather than applied silently.

---

### User Story 2 - A Vercel production deployment is a change (Priority: P1)

The organisation's front end deploys on Vercel. A production deployment reaches `READY` at
14:41; the feeder turns it into a ROLLOUT change node against the service the project maps to,
valid at the instant the deployment became the production deployment, with the git commit it
carried as a claim — the **same** claim key GitHub mints — so that the GitHub run that triggered
it and the Vercel deployment that served it resolve into one change. Preview deployments are
excluded by the environment filter and the filter is stated.

**Why this priority**: Vercel shipped the other of the two incidents in the audit that this
feature covers, and it is the platform where a change is most invisible today: nothing outside
Vercel records that production moved.

**Independent Test**: Replay the recorded Vercel deployment payloads for an incident window from
an empty graph and compare the change nodes, targets, actors, actor kinds, valid times and claims
to the golden output; check that no preview deployment produced a change and that the exclusion
is counted.

**Acceptance Scenarios**:

1. **Given** recorded Vercel deployments over a window containing production and preview
   deployments, **When** the feeder runs, **Then** exactly the production deployments become
   ROLLOUT change nodes, each valid at the instant it became the production deployment, each with
   a link back to the deployment, and the number of deployments excluded by the environment filter
   is reported.
2. **Given** a production deployment built from a git commit, **When** its claims are inspected,
   **Then** it carries `deploy.commit_sha` with the same value the GitHub feeder would mint for
   the same commit, plus a `vercel.project` reference and a `github.repo` claim where Vercel
   reports the connected repository.
3. **Given** the same commit deployed by a GitHub Actions run and served by a Vercel production
   deployment, **When** both sources have been ingested, **Then** the published certain rules
   merge them into one change node rather than producing two adjacent entries in the ranked list.
4. **Given** a deployment triggered by a person's push, one triggered by the Vercel CLI in CI,
   and one created by an integration, **When** all three are ingested, **Then** each carries an
   actor kind the platform states, and `unknown` with the available evidence where it does not.
5. **Given** a deployment that is built long before it is promoted, **When** it is promoted to
   production, **Then** the rollout's valid time is the **promotion** instant, because that is
   when the production system changed, and the build instant is recorded as a property rather
   than used as valid time.

---

### User Story 3 - Environment and project configuration changes (Priority: P2)

Someone changes a production environment variable on Vercel at 03:10 and nothing deploys until
the next morning, when the incident starts. The feeder turns the variable change into a CONFIG
change node naming the **key** and the environment it applies to and the version the platform
assigns — never the value, not even encrypted, not even hashed — so the investigation engine can
see that a configuration changed and when, and so the next rollout can be read as the moment it
took effect.

**Why this priority**: Configuration changes are a documented incident cause and are invisible
everywhere else, but the graph is useful before they exist and their mapping is the one most
likely to differ between platforms.

**Independent Test**: Replay recorded environment-variable change payloads and compare the CONFIG
change nodes, their keys, environments, targets and valid times to the golden output; assert over
the whole corpus that no variable value, ciphertext or hash of a value appears in any event.

**Acceptance Scenarios**:

1. **Given** a production environment variable created, updated or removed, **When** the feeder
   runs, **Then** a CONFIG change node exists carrying the variable **key**, the environment it
   applies to, the operation, the version identifier the platform assigns, the actor and actor
   kind, and a `changed-by` edge to the service the project maps to.
2. **Given** any environment-variable change, **When** every event the feeder emitted is
   inspected, **Then** no value, no encrypted value, no truncated value and no hash of a value is
   present in any property, claim or event body, and the connector never requests the decrypted
   value.
3. **Given** a variable changed at 03:10 and a production deployment at 09:05, **When** the
   investigation engine asks what changed before 09:30, **Then** both appear, the configuration
   change at 03:10 and the rollout at 09:05, and neither is folded into the other.
4. **Given** a variable scoped to preview environments only, **When** the feeder runs, **Then** it
   is excluded by the same environment filter that excludes preview deployments, and the exclusion
   is counted.

---

### User Story 4 - The organisation's own payloads, sanitised, are the tests (Priority: P2)

Casey runs a recording campaign against the organisation's GitHub and Vercel: a baseline window,
the two incident windows the audit identified, a window containing a rate-limit response, and a
window containing a poll that failed part-way. Every recording is sanitised in the connector
before anything touches disk, scanned independently at commit, signed off, and committed to the
**private** corpus; the public repository carries **synthetic structural twins**.

**Why this priority**: Constitution VIII says a connector with synthetic-only test data cannot be
marked stable — but both feeders can be written and reviewed before the campaign runs.

**Independent Test**: The committed fixtures replay from empty to their goldens, double-deliver
as a no-op, shuffle inside the declared reordering window without changing valid-time state, and
pass the secret and personal-data scan. Separately, the public repository's synthetic twins
replay to their own goldens with the private corpus absent.

**Acceptance Scenarios**:

1. **Given** a recorded campaign, **When** the fixtures are verified, **Then** each replays from
   empty to its golden graph, double delivery changes nothing, and shuffling inside the declared
   window changes no valid-time state.
2. **Given** a recording containing user logins, email addresses, repository names, deployment
   URLs and commit messages, **When** it is sanitised, **Then** every people identifier is
   **dropped, not hashed**; every infrastructure and repository identifier is replaced by a keyed
   HMAC pseudonym that is the same everywhere it occurs in the corpus, so the claim joins that
   prove "one rollout, not two" survive sanitisation; and commit messages and pull-request titles
   are dropped as free text.
3. **Given** a sanitised recording, **When** it is committed, **Then** it carries a signed
   manifest naming the sanitisation policy version, the content hash, who signed it off and when,
   and what was dropped.
4. **Given** canary tokens seeded into the source data before the campaign, **When** the recording
   is committed, **Then** an independent secrets-and-entropy scan finds none of them, and a
   surviving canary fails the commit rather than raising a warning.
5. **Given** the public repository alone, **When** the conformance suite runs, **Then** it passes
   against the synthetic twins and no real production recording is present.

---

### User Story 5 - Rollbacks and promotions (Priority: P3)

At 15:02 someone rolls production back to the previous Vercel deployment, or re-promotes an
earlier build. The feeder emits that as a ROLLOUT change like any other, flagged as a rollback
**because the platform said so**, carrying the deployment it went back to. The investigation
engine can then see both the change that broke production and the change that stopped the
bleeding, and the timeline reads correctly.

**Why this priority**: A rollback is evidence, not a cause, and the engine needs it to close an
investigation — but an investigation is still gradeable without it, and the flag is only
trustworthy on platforms that state it, which is a small part of the corpus.

**Independent Test**: Replay a recorded window containing a deploy, an incident and a rollback,
and check that three changes exist in the right order with the rollback flagged and pointing at
the deployment it restored.

**Acceptance Scenarios**:

1. **Given** a Vercel rollback or promotion of an earlier deployment to production, **When** the
   feeder runs, **Then** a ROLLOUT change exists at the instant production moved, flagged as a
   rollback, naming the deployment restored, with the actor and actor kind the platform states.
2. **Given** a re-deployment of an earlier commit where the platform does **not** say it is a
   rollback, **When** the feeder runs, **Then** the change is **not** flagged: the flag is never
   inferred from commit ordering, from a revert-shaped commit message or from the deployment
   being older than the one it replaced.
3. **Given** a rollback and the original rollout, **When** the investigation engine asks for the
   window, **Then** both appear with their own valid times, the rollback is distinguishable from
   the rollout it undoes, and the rollback is not ranked as a candidate cause of an incident that
   began before it.

---

### Edge Cases

- **A monorepo run that deploys several services.** **Decided: several changes, one per target.**
  The unit of a rollout change is **(origin run, target)**, not the run: a workflow run that
  deploys three services produces three ROLLOUT change nodes sharing one origin reference and one
  `deploy.commit_sha` claim, each with its own target and its own `deploy.image` / `deploy.release`
  claim where the run names one. The reason is the merge: the platform observes rollouts one per
  service (a Cloud Run revision, a Kubernetes ReplicaSet), so a one-to-many change would have to
  be merged against many one-to-one changes, which no published certain rule can do without
  guessing. A one-change-per-target model merges pairwise and lets the ranker exonerate the two
  services in the run that are not implicated. Where the run names no per-target artefact and the
  targets come only from the declared repository↔service mapping, the same rule applies over that
  mapping; where nothing names a target, one unattached change is emitted for the run.
- **A deployment to a preview or ephemeral environment.** Excluded by the environment filter —
  a preview URL is not the production system. The exclusion is **counted and stated in the
  checkpoint**, never silent, so a later reader can tell "nothing deployed" from "we were not
  looking at that environment".
- **A re-run of a workflow.** A re-run is a new attempt of the same run identifier. If it produces
  a deployment that completes, the production system changed again and it is a **new** change,
  keyed on (run identifier, attempt, target); the earlier change is not amended or retracted. A
  re-run that fails, or that deploys nothing, produces no change.
- **A deployment that never reaches a terminal state.** Queued, in progress, or abandoned. No
  change node until a terminal successful state exists, because nothing in production has moved.
  A deployment that ends in failure is recorded as a failed attempt only where the platform
  states it, and never as a ROLLOUT.
- **A deployment with an unknown target.** Kept as an unattached change with every identifier it
  carried recorded as claims, and attached automatically when the target appears — because a
  service is frequently created in the graph by the platform feeder minutes after its first
  deploy.
- **A webhook replay, a forged signature or a malformed body.** The endpoint is a doorbell: the
  signature is verified, the body is never read as data, and the worst an attacker who guesses
  the URL and holds the secret can achieve is one extra poll inside the configured rate limit.
  A notification that fails signature verification is dropped and counted.
- **The same rollout seen by this feeder and by a platform feeder.** Both are true. This feeder
  emits its own change with every shared identifier as a claim and **never merges anything
  itself**; the published certain rules collapse the pair. Where no shared identifier exists the
  pair is raised as a resolution suggestion, never merged. The failure this guards against is two
  adjacent entries for one rollout in the ranked list.
- **A commit that no longer exists.** A force-push or a deleted branch can leave a deployment
  whose commit is unreachable. The `deploy.commit_sha` claim is kept as stated by the deployment
  record; the feeder does not verify reachability and does not drop the claim.
- **A repository or project renamed.** The platform's stable numeric identifier is what the
  reference is keyed on; the name is a property and a claim. A rename is a property change, never
  a deletion and a creation.
- **A release published without a deployment, or a deployment without a release.** Both are
  normal. A release is a rollout only where the organisation's configuration says a release is
  how that repository ships; otherwise it is a change of kind `other` with the vendor's own kind
  recorded, never dropped.
- **Two production deployments completing within the same second.** Each is its own change with
  its own identifier; the ordering the platform states is preserved and no tie is broken by
  guessing. Where the platform states equal instants, both are emitted and the ambiguity is
  recorded.
- **A credential that turns out to be write-capable.** The feeder refuses to start and names the
  scopes or permissions that grant write, in the same shape feature 001's Kubernetes feeder uses.
  Where the platform cannot report a permission, the feeder names what it could not verify and
  requires the operator to assert read-only in configuration.
- **A rate limit or a pagination failure in the middle of a poll.** The feeder emits no retraction
  derived from a partial read, checkpoints only the extent it completed, and declares the gap.
- **Clock skew between the platform and the graph.** Valid time comes from the platform's
  timestamps and observed time from the graph. Neither corrects the other; skew beyond a stated
  threshold is reported in the feeder's own operational telemetry.
- **A recording that would leak a customer identifier or a secret.** A payload that cannot be
  sanitised is dropped from the recording and the drop is documented. A fixture is never committed
  on the promise of being cleaned later.

## Requirements *(mandatory)*

### Functional Requirements

#### A. Two feeders, one mapping, read-only credentials

- **FR-001**: This feature MUST deliver exactly two feeders — one for GitHub, one for Vercel —
  over **one published deployment-as-change mapping**. Neither feeder MUST implement a telemetry
  backend: both emit graph events and write pointers, and neither executes a pointer or returns a
  digest.
- **FR-002**: Both feeders MUST use read-only credentials and MUST declare the exact read
  permissions they require, per area read: for GitHub, a fine-grained personal access token or a
  GitHub App installation granting read-only access to metadata, actions, deployments, contents
  and releases; for Vercel, a token scoped to the team with read access to projects and
  deployments. A credential granting any write permission MUST be refused.
- **FR-003**: At startup each feeder MUST ask the platform what its credential is permitted to do
  and MUST refuse to start when the credential grants write access in any area it reads, naming
  the offending permission or scope. Where the platform cannot report a permission, the feeder
  MUST name the part it could not verify and MUST require the operator to assert read-only in
  configuration; it MUST NOT assume.
- **FR-004**: Neither feeder MUST be capable of issuing any operation that changes platform state
  — dispatching, re-running or cancelling a workflow, creating or updating a deployment or a
  deployment status, creating or editing a release, promoting, rolling back, redeploying,
  cancelling or deleting a deployment, changing an environment variable or a project setting,
  writing a comment, a check or a commit status — and this MUST be verifiable by inspecting the
  set of operations the feeder can issue, not only its behaviour on one run.
- **FR-005**: Neither feeder MUST request, receive or store a **secret value**: GitHub Actions
  secret and variable values, and Vercel environment variable values in plaintext or ciphertext,
  MUST never be read. Only key names, scopes, versions and change metadata are in scope
  (FR-038).
- **FR-006**: Both feeders MUST be usable in two modes with identical output: live against the
  platform, and offline against recorded payloads. Recorded mode is the test. Nothing in any
  event may derive from the connection itself — only from a payload or from configuration.
- **FR-007**: Each GitHub organisation and each Vercel team MUST be a distinct source with its own
  source identifier, credential, budget and checkpoints. One feeder instance MUST NOT mix two
  organisations or two teams into one source.
- **FR-008**: The scope each feeder reads MUST come from **what the credential was granted**, not
  from a list the connector maintains alongside it. For GitHub the scope is the **App
  installation's repository selection** — the owner chooses, at install time and afterwards, which
  repositories this application may see — and the feeder MUST **enumerate that selection from the
  platform at startup** rather than hold its own allowlist. Where a platform cannot express a
  per-target grant, the targets MUST be configuration within what the credential does grant, and
  the feeder MUST say which of the two it is operating under.
  The scope in force MUST be recorded in the checkpoints, so a query can tell "nothing deployed"
  from "not in scope at that time", and it MUST be changeable — by changing the grant — without
  losing history.
- **FR-009**: Both feeders MUST emit their own operational telemetry: calls made per platform area,
  rate-limit responses, poll duration and lag, events emitted and rejected, deployments excluded
  by each filter, and observed clock skew between platform timestamps and graph observed time.
- **FR-010**: Both feeders MUST be built on feature 001's published feeder contract and MUST
  declare their source identifier, ordering guarantee, reordering window, required read scopes and
  the complete set of identifier namespaces that appear in any reference they emit. A reference
  outside the declared set MUST fail the run.

#### B. The shared deployment-as-change mapping

- **FR-011**: A completed deployment MUST become a **ROLLOUT change node**, and the unit MUST be
  **(origin run or deployment, target)**: one change node per deployed target, never one change
  node carrying several targets (the monorepo decision, FR-017).
- **FR-012**: A rollout change's **valid time MUST be the instant the deployment completed** as
  the platform states it — the transition to a terminal successful state, or the instant the
  deployment became the production deployment where the platform distinguishes build from
  promotion. It MUST NOT be the run's start instant, the build instant, or the instant the feeder
  learned of it. Where the platform states no completion instant, the valid start MUST be marked
  **unknown**; it MUST NOT be guessed.
- **FR-013**: Every change node either feeder creates MUST carry an **actor** — the triggering
  user, application or integration the platform names — and an **actor kind** from feature 001's
  published set (ADR-0005 D1), whose membership is exactly `PERSON` ("human-originated"),
  `AUTOMATION`, `CONTROLLER`, `VENDOR` ("vendor-originated") and `UNKNOWN`. Within what this
  specification previously called "system-originated", the split is normative and MUST be applied:
  **a CI run or a bot account is `AUTOMATION`** — machinery acting on a person's behalf, whose
  change is still a candidate cause — while **an autoscaler or a scheduler is `CONTROLLER`** —
  machinery reacting to state, which feature 002's causal ordering may exonerate after onset.
  The actor kind MUST be derived from what the platform states about the trigger and the account
  type, and MUST NOT be inferred from the actor's name. Where it cannot be derived, it MUST be
  `UNKNOWN` with the evidence that was available recorded.
- **FR-014**: Every change node MUST carry an **origin reference**: the platform URL of the
  workflow run, deployment or release that produced it, emitted both as the change's origin and as
  a `SOURCE_LINK` pointer. The origin reference MUST carry no credential and no token.
- **FR-015**: Every change node MUST carry `changed-by` edges to the targets it resolves, and MUST
  carry, as identity claims, every identifier the platform states that another source could also
  know (FR-041).
- **FR-016**: A rollout MUST be flagged **`rollback=true`** only when the platform states that the
  action was a rollback or a promotion of an earlier deployment. The flag MUST NOT be inferred
  from commit ordering, from the deployment being older than the one it replaced, or from the
  shape of a commit message. Where the platform states it, the change MUST also name the
  deployment or release that was restored.
- **FR-017**: A run that deploys several targets MUST produce **one change node per target**,
  sharing one origin reference and one `deploy.commit_sha` claim, each with its own target and its
  own artefact or release claim where the run names one. Where a run names no per-target artefact,
  the targets MUST come from the declared repository↔service mapping, which is **connector
  configuration owned by the operator** and MUST NOT be read from a file committed in the
  repository being observed.
- **FR-018**: A change whose target cannot be resolved MUST be kept and marked **unattached**,
  with every unresolvable identifier recorded, and MUST attach automatically when a matching
  target later appears.
- **FR-019**: A run, deployment or release that does not change the production system — a test or
  lint run, a deployment that never reaches a terminal successful state, a deployment to an
  environment outside the filter — MUST produce **no change node**, and the filter that excluded
  it MUST be counted and stated in the checkpoint rather than applied silently.
- **FR-020**: Event identifiers MUST be a pure function of the payload, and the idempotency key of
  a rollout MUST be (source, origin run or deployment identifier, attempt, target), so that
  re-delivery by either transport, or a re-read of the same object, is a no-op.

#### C. GitHub

- **FR-021**: The GitHub feeder MUST read, for in-scope repositories: workflow runs, deployments
  and their deployment statuses, and releases. It MUST NOT read pull requests, reviews, check
  runs, test results, code contents or diffs (Out of scope).
- **FR-022**: A GitHub deployment MUST become a rollout change when its **deployment status
  reaches a terminal successful state**, with that transition's instant as valid time. Subsequent
  status transitions on the same deployment — for example a transition to inactive when a later
  deployment supersedes it — MUST be recorded as properties of the same change, never as new
  rollout changes.
- **FR-023**: Which GitHub **environments** count as production MUST be configurable as an
  allowlist, and the allowlist in force MUST be recorded in the checkpoint. A deployment to an
  environment outside it MUST be excluded and counted (FR-019).
- **FR-024**: A workflow run that produced no GitHub deployment object MUST still become a rollout
  change where the run is on the configured deploy-workflow allowlist and completed successfully,
  with the run's completion instant as valid time. A run outside that allowlist MUST produce no
  change.
- **FR-025**: A **release** MUST become a rollout change where the organisation's configuration
  states that the repository ships by release; otherwise it MUST become a change of the taxonomy's
  "other" kind with GitHub's own object kind recorded as a property, and MUST NOT be dropped.
- **FR-026**: The GitHub feeder MUST mint a `github.repo` identity claim for the repository behind
  every change, valued `<owner>/<repository>`, with the repository's stable platform identifier as
  a supporting attribute so that a rename is a property change and not a new entity.
- **FR-027**: The GitHub feeder MUST derive actor kind from what GitHub states: a run triggered by
  a push or a manual dispatch performed by a user account is `PERSON`; a run triggered by a
  **schedule** is `CONTROLLER`; a run triggered by a repository dispatch or by another workflow is
  `AUTOMATION`; an actor that is an application or a **bot** account is `AUTOMATION`. The account
  type MUST come from the payload, never from the login string.
- **FR-028**: A **re-run** of a workflow MUST produce a new change when it deploys, keyed on the
  run identifier **and the attempt number**; the earlier change MUST NOT be amended or retracted.
- **FR-029**: The GitHub feeder MUST write a `LOG` pointer to the run's job logs where the run
  produced them, and a `SOURCE_LINK` pointer to the run, deployment or release. It MUST NOT fetch
  log content (FR-001).
- **FR-030**: The GitHub feeder MUST NOT create a node for a repository as a graph entity in its
  own right in v1: a repository is an identity claim on the service it ships, so that the graph
  does not acquire a parallel code-organisation taxonomy.

#### D. Vercel

- **FR-031**: The Vercel feeder MUST read, for in-scope projects: deployments and their states,
  promotions and rollbacks, project configuration metadata, and environment-variable change
  metadata. It MUST NOT read deployment build output, source files, analytics or web-vitals data.
- **FR-032**: Only deployments that are or become the **production deployment** MUST become
  rollout changes. Preview and ephemeral deployments MUST be excluded by the environment filter,
  counted, and stated in the checkpoint (FR-019).
- **FR-033**: A deployment that is built at one instant and promoted at another MUST take the
  **promotion instant** as the rollout's valid time, because that is when the production system
  changed; the build instant MUST be recorded as a property.
- **FR-034**: A Vercel **promotion or rollback** MUST become a rollout change flagged per FR-016,
  naming the deployment restored.
- **FR-035**: The Vercel feeder MUST mint a `vercel.project` reference for the project, and MUST
  mint a `github.repo` claim where Vercel states the connected repository, so that a Vercel
  project and a GitHub repository that ship the same service meet as claims about one entity.
- **FR-036**: The Vercel feeder MUST mint `deploy.commit_sha` for the commit a deployment carried,
  using **the same claim key and the same value form** the GitHub feeder uses, so that the GitHub
  run and the Vercel deployment of one commit resolve to one change.
- **FR-037**: The Vercel feeder MUST derive actor kind from what Vercel states about the
  deployment's trigger: a git push or a dashboard action by a user is `PERSON`; a CLI-in-CI,
  integration or automatic deployment is `AUTOMATION`; a deployment Vercel attributes to a
  scheduled or platform-initiated job is `CONTROLLER`; where Vercel does not state it, `UNKNOWN`.
- **FR-038**: An environment-variable creation, update or removal MUST become a **CONFIG change
  node** carrying the variable **key**, the environments it applies to, the operation, the version
  or revision identifier the platform assigns, the actor and actor kind, and a `changed-by` edge
  to the service the project maps to. It MUST NOT carry the value in any form — not plaintext, not
  ciphertext, not truncated, not hashed — and the feeder MUST NOT request the decrypted value
  (FR-005).
- **FR-039**: A configuration change MUST be emitted at the instant the platform states the
  variable changed, independently of when a deployment next applied it. The feeder MUST NOT fold
  a configuration change into the following rollout and MUST NOT delay it until one occurs.
- **FR-040**: The Vercel feeder MUST write a `LOG` pointer to the deployment's build and runtime
  logs and a `SOURCE_LINK` pointer to the deployment, and MUST NOT fetch log content.

#### E. Identity, claims and "one rollout, not two"

- **FR-041**: Both feeders MUST mint, on every change they create, **every identifier another
  source could also know**, as identity claims, with the supporting attributes a resolution rule
  needs. The claim keys are published and MUST be exactly:
  - **`deploy.commit_sha`** — the full commit identifier the rollout shipped, as the platform
    states it, unabbreviated and case-normalised.
  - **`deploy.image`** — the artefact identifier the rollout shipped, preferring an immutable
    digest form over a mutable tag where the platform states both.
  - **`deploy.release`** — the release, version or tag name the rollout shipped.
  A claim MUST be omitted rather than invented where the platform states no value for it.
- **FR-042**: Both feeders MUST mint the **target-side** claims that link the shipping system to
  the running one: `github.repo` for the repository (FR-026), `vercel.project` for the project
  (FR-035), plus the environment as a supporting attribute on every claim, because a repository
  deploying to staging is not the same target as the same repository deploying to production.
- **FR-043**: Neither feeder MUST merge anything itself, and neither MUST suppress its own
  observation because another source may have reported the same rollout. Resolution is the graph's
  (constitution VI).
- **FR-044**: Where two sources report the same rollout and no published rule can merge them, the
  pair MUST appear as a **resolution suggestion** with its score and rationale, never as a silent
  merge and never as two unrelated changes with no indication that they may be one.
- **FR-045**: Both feeders MUST declare and publish their identifier namespaces before use, and
  MUST NOT reuse an existing published namespace for something it does not mean. At minimum:
  `github.repo`, `github.change`, `vercel.project`, `vercel.change`, and the three cross-source
  deploy claim namespaces of FR-041.
- **FR-046**: Where a claim's value form differs between platforms for the same real-world
  identifier, the **published normalisation** MUST be applied before the claim is minted, so that
  two sources naming one commit or one artefact produce byte-identical claim values. The
  normalisation MUST be published with the claim keys and MUST NOT be applied privately by one
  feeder.

#### F. Pointers

- **FR-047**: Every node and change either feeder creates MUST carry pointers: a `SOURCE_LINK` to
  the run, deployment, release or configuration object it came from, and a `LOG` pointer where the
  platform offers logs for it.
- **FR-048**: A pointer MUST name the **backend family** only (`github`, `vercel`). It MUST
  contain no organisation identifier beyond what the link requires, no account, no token and no
  URL that embeds one.
- **FR-049**: The attributes that identify **which entity** a pointer is about MUST be expressed in
  OpenTelemetry semantic conventions, and the pointer's vocabulary MUST be a registered, versioned
  name. Where a selector cannot be expressed in OpenTelemetry terms, the published documentation
  MUST state why (constitution IV).
- **FR-050**: Pointers MUST be versioned in valid time with the node or change they belong to, and
  MUST be **additive**: a deploy feeder's pointers MUST NOT replace or suppress pointers another
  source attached to the same entity.
- **FR-051**: Neither feeder MUST execute a pointer it writes. Executing these pointers, if it is
  ever done, is a telemetry-backend feature and belongs to a later increment.

#### G. Transports: polling is the truth, a webhook is a doorbell

- **FR-052**: **Polling is the source of truth** for both platforms, at a configurable interval
  per area. A polled history MUST be marked as sampled at the poll interval, so a consumer can
  tell a complete history from a sampled one.
- **FR-053**: An inbound webhook MUST act only as a **doorbell**: it MUST enqueue "poll now" and
  MUST NOT be a source of data. The feeder MUST re-read the object from the platform API and MUST
  NOT parse, trust or store the notification body.
- **FR-054**: Every inbound notification MUST be **HMAC signature-verified** against the shared
  secret the platform signs with, using a constant-time comparison; a notification that fails
  verification, or that carries no signature, MUST be dropped and counted. The endpoint MUST be
  rate limited, so a forged, replayed or malformed notification costs at most one extra poll and
  can never create, alter or retract a node, an edge, a change or an event.
- **FR-055**: Webhook and polling MUST be two transports for the same event and MUST be usable
  together — push for speed, pull for truth. Running both MUST NOT produce two changes, and the
  result MUST NOT depend on the order in which the transports deliver (FR-020).
- **FR-056**: A poll that fails part-way MUST emit no retraction for the unread part and MUST
  checkpoint only the extent it completed, declaring the gap.
- **FR-057**: Both feeders MUST emit a source checkpoint on start, on resync and after any gap,
  stating the extent actually covered, the filters and scope in force, and whether the feeder was
  watching immediately before that extent.
- **FR-058**: Where the platform's timestamps and the graph's observed time disagree by more than
  a stated threshold, the skew MUST be reported in operational telemetry. Neither MUST be
  corrected with the other.

#### H. Recordings, sanitisation and evaluation

- **FR-059**: The test corpus MUST be **recorded real payloads** from the organisation's GitHub and
  Vercel, sanitised, committed as fixtures with their resulting event streams and golden query
  outputs. Synthetic-only data is insufficient for either feeder to be marked stable
  (constitution VIII).
- **FR-060**: Real production recordings MUST NOT be committed to the **public** repository. The
  sanitised corpus MUST live in a private repository with private CI, and the public repository
  MUST carry **synthetic structural twins** — the same event shapes and sequences with no
  identifier derived from the organisation. The public conformance suite MUST run against the
  twins and the private corpus MUST run the same suite.
- **FR-061**: The campaign MUST cover at minimum: a baseline window; the two incident windows the
  coverage audit identified, each containing the deployment that caused it; a window containing a
  monorepo run that deployed several targets; a window containing a rollback; a window containing
  a rate-limit response; and a window containing a poll that failed part-way.
- **FR-062**: Sanitisation MUST happen **in the connector, before anything touches disk**. No
  unsanitised payload may reach disk, a log or any artifact at any point, including during a
  failed or aborted run.
- **FR-063**: The sanitisation contract MUST state, per field, exactly one of: recorded verbatim
  on an allowlist, replaced by a keyed pseudonym, or dropped. **People identifiers — logins,
  display names, email addresses, avatars — MUST be dropped, never hashed.** Repository, project,
  organisation, team, deployment and host identifiers MUST be pseudonymised with a **keyed HMAC**,
  consistently across the whole corpus, so that the claim joins on which "one rollout, not two"
  depends survive sanitisation. **Commit messages, pull-request titles and any other free text
  MUST be dropped.** Commit identifiers MUST be pseudonymised rather than dropped, for the same
  reason.
- **FR-064**: Every committed recording MUST carry a **signed manifest** stating the sanitisation
  policy version, the content hash of what was signed, the named individual who signed it off, the
  date, and what was dropped. A payload that cannot be made safe MUST be dropped and the drop
  documented.
- **FR-065**: Every committed artifact MUST pass an **independent secrets-and-entropy scan**
  implemented separately from the sanitiser, and **canary tokens** seeded into the source data
  before a campaign MUST be asserted absent from every committed artifact; a surviving canary MUST
  fail the commit rather than raise a warning.
- **FR-066**: Every fixture MUST satisfy the published conformance suite: replay from empty to its
  golden graph, double delivery as a no-op, and shuffling within the declared reordering window
  leaving valid-time state unchanged.
- **FR-067**: Both feeders MUST pass a **zero-payload check**: over the whole recorded corpus,
  every event emitted passes the graph's telemetry-payload validation, and at least one fixture
  MUST contain a payload that must always be refused so the rejection path is exercised.
- **FR-068**: A **live parity check** MUST be documented as in feature 001's live run: the graph
  built live against the organisation MUST equal the graph built from the recording of the same
  run.
- **FR-069**: Coverage MUST be measurable against the platforms' own answers: the set of completed
  in-scope deployments each platform lists for a window compared with the set of rollout changes
  produced for it, with the differences enumerated rather than summarised.
- **FR-070**: At least one fixture MUST pair a deploy feeder's recording of a rollout with a
  platform feeder's recording of the same rollout (feature 003's Cloud Run or feature 001's
  Kubernetes), so that the "one rollout, not two" property is pinned by a test rather than
  asserted (SC-004).

#### I. Budget, rate limits and operability

- **FR-071**: Each feeder MUST have a configurable call budget and MUST plan each polling cycle to
  stay inside it. Where the platform reports remaining quota in its response headers, the budget
  MUST be expressed as a **share of the remaining quota**, and the feeder MUST reduce its own
  allowance as the remaining quota falls; where it does not, the feeder MUST fall back to the
  configured static budget and MUST say in its usage report that it did so.
- **FR-072**: The quota is shared with the humans and the automation of the organisation. Each
  feeder MUST leave a published reserve unspent and MUST yield rather than compete when the
  remaining quota falls below it.
- **FR-073**: When the budget cannot cover a full cycle, the feeder MUST defer work in a
  **published** priority order, MUST report what it deferred and why, and MUST reflect the reduced
  coverage in its checkpoints. Stopping for quota MUST be a typed reason, distinct from having
  found nothing.
- **FR-074**: Each feeder MUST honour the platform's rate-limit responses, waiting at least as long
  as the response asks, and MUST resume from where it stopped rather than restarting the cycle.
- **FR-075**: Each feeder MUST report its usage: calls per platform area, share of the configured
  budget consumed, share of the remaining platform quota taken, rate-limit responses received, and
  how much of the published reserve was left untouched.
- **FR-076**: Each feeder MUST document what it costs to run against an estate of a stated size —
  calls per hour per area at the default cadence — so the platform owner can approve it before it
  runs.

### Data mapping *(what a platform object becomes)*

Observed time is assigned by the graph at acceptance in every row. "Unknown start" means the
feeder MUST mark the valid start unknown rather than guess.

| Platform object | Graph element | Identity and refs | Valid time | Learning cadence and gaps |
|---|---|---|---|---|
| GitHub deployment whose status reached a terminal success, per target | CHANGE node, kind ROLLOUT, with actor, actor kind, origin link, `changed-by` edge per target | ref in `github.change`, keyed by repository, deployment identifier, attempt and target; claims `deploy.commit_sha`, `deploy.image`, `deploy.release`, `github.repo` | the instant of the terminal success transition | deployments and statuses poll, optionally woken by a signature-verified doorbell; a partial read checkpoints a gap and retracts nothing |
| GitHub workflow run on the deploy-workflow allowlist with no deployment object | CHANGE node, kind ROLLOUT, per resolved target | ref in `github.change`, keyed by run identifier, attempt and target | the run's completion instant | workflow-runs poll |
| GitHub workflow run not on the allowlist (tests, lint, chores) | **nothing — excluded and counted** | — | — | the exclusion is stated in the checkpoint |
| GitHub release | CHANGE node, kind ROLLOUT where the repository ships by release, otherwise kind `other` with GitHub's own kind recorded | ref in `github.change`, keyed by release identifier; claim `deploy.release` | the publication instant | releases poll |
| GitHub repository | **an identity claim, not a node** | claim `github.repo`, value `<owner>/<repository>`, with the stable platform identifier as a supporting attribute | with the change or target it supports | with the object that named it |
| Vercel production deployment | CHANGE node, kind ROLLOUT, with actor, actor kind, origin link, `changed-by` edge to the service | ref in `vercel.change`, keyed by deployment identifier; claims `deploy.commit_sha`, `vercel.project`, `github.repo` where stated | the instant it became the production deployment | deployments poll, optionally woken by a doorbell |
| Vercel preview or ephemeral deployment | **nothing — excluded and counted** | — | — | the exclusion is stated in the checkpoint |
| Vercel promotion or rollback | CHANGE node, kind ROLLOUT, `rollback=true` where Vercel states it, naming the deployment restored | ref in `vercel.change`, keyed by the promotion event | the instant production moved | deployments and events poll |
| Vercel environment variable created, updated or removed | CHANGE node, kind CONFIG, carrying the **key**, the environments, the operation and the platform's version identifier | ref in `vercel.change`, keyed by variable identifier and version | the instant the platform states the variable changed | environment poll |
| Vercel project | SERVICE target reached through claims, plus a `vercel.project` reference | ref in `vercel.project`; claim `github.repo` where the repository is connected | with the change it supports | with the project poll |
| Commit, artefact and release identifiers | **identity claims on the change**, never nodes | `deploy.commit_sha`, `deploy.image`, `deploy.release` | with the change | with the change |
| Job logs, build logs, runtime logs | **pointers on the change or node**, never nodes and never content | — | with the node or change version | with the object |
| Secret and environment-variable **values**, code diffs, test results, review comments | **nothing — never read** | — | — | — |

### Key Entities

- **Rollout change**: one completed deployment of one target, with a valid time taken from the
  platform's completion instant, an actor, an actor kind, an origin link, a rollback flag and the
  deploy claims. The unit of this feature.
- **Configuration change**: one environment-variable or project-setting change, carrying the key
  and never the value.
- **Deploy claim**: `deploy.commit_sha`, `deploy.image` or `deploy.release` — an identifier that
  another source observing the same rollout can also state, which is what makes one rollout one
  change node.
- **Target claim**: `github.repo` or `vercel.project`, with the environment as a supporting
  attribute — what links the shipping system to the running one.
- **Actor kind**: one member of feature 001's published set (ADR-0005 D1) — `PERSON` ("a person"),
  `AUTOMATION` (CI or a bot acting on a person's behalf), `CONTROLLER` (a scheduler, autoscaler or
  platform automation reacting to state), `VENDOR` ("the vendor") or `UNKNOWN` ("something the
  feeder cannot type").
- **Origin reference**: the platform URL of the run, deployment, release or configuration object
  that produced a change — the thing an SRE opens.
- **Environment filter**: the declared set of environments that count as production, its
  exclusions counted and stated in the checkpoint.
- **Doorbell notification**: a signature-verified inbound message whose only effect is to trigger
  a poll, whose body is never read.
- **Recording campaign**: the recorded windows, their sanitisation record, the sign-off and what
  was dropped.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For every in-scope repository, at least 99% of GitHub deployments that reached a
  terminal successful state in a window appear as rollout change nodes, and every difference
  between the two sets is enumerated with a reason.
- **SC-002**: For every in-scope project, 100% of Vercel production deployments in a window appear
  as rollout change nodes, **zero** preview deployments produce a change, and the number excluded
  by the environment filter is reported on every cycle.
- **SC-003**: A rollout's valid time equals the completion instant the platform stated, exactly, in
  100% of cases across the recorded corpus; and the delay from that instant to the change being
  queryable is at or below the configured poll interval plus one minute at the 95th percentile
  with polling alone, or one minute at the 95th percentile when a doorbell is configured as well.
- **SC-004**: **One rollout, not two.** A rollout observed by a deploy feeder and by a platform
  feeder — Cloud Run in feature 003 or Kubernetes in feature 001 — appears exactly once in the
  ranked change list, in the fixture recorded for that case; zero cases of two adjacent entries
  for one real-world rollout across the corpus.
- **SC-005**: 100% of change nodes carry an actor kind; on the labelled fixture set at least 95%
  of changes the platform types are typed correctly as `PERSON`, `AUTOMATION` or `CONTROLLER`; and zero
  actor kinds are derived from the actor's name string, verified by a fixture in which a human's
  login resembles a bot's and vice versa.
- **SC-006**: **Zero secret exposure**: over the whole recorded corpus and the live run, zero
  environment-variable values, ciphertexts, truncations or hashes of values appear in any event,
  property, claim or fixture, and zero requests for a decrypted value are issued.
- **SC-007**: **Zero writes**: every request either feeder is capable of issuing is on a published
  read-only list, verified from the feeders' own recorded request logs over the full corpus and
  the live run.
- **SC-008**: Zero telemetry payloads reach the graph: 100% of emitted events pass the
  telemetry-payload validation over the whole corpus, and the rejection path is exercised by at
  least one fixture.
- **SC-009**: 100% of rollback flags are set only where the platform stated a rollback or a
  promotion; zero are inferred, verified against a fixture containing a redeploy of an older
  commit that the platform does not call a rollback.
- **SC-010**: A run that deploys N targets produces exactly N rollout change nodes, each with
  exactly one target, all sharing one origin reference and one `deploy.commit_sha` claim, in the
  monorepo fixture.
- **SC-011**: Every shipped fixture replays from empty to its goldens, double-delivers as a no-op,
  and shuffles within the declared reordering window without changing valid-time state, on every
  verification run, with zero tolerance.
- **SC-012**: A forged, replayed or malformed doorbell notification costs at most one poll and zero
  graph writes: across the adversarial fixture set, 100% produce no node, no change and no event,
  and the polls they induce never exceed the configured rate limit.
- **SC-013**: A full polling cycle over the in-scope estate costs no more than the documented
  number of calls per hour per area, never exceeds its configured share of the remaining platform
  quota, leaves the published reserve unspent in 100% of cycles, and its usage report matches the
  number of calls in the recording to the exact call.
- **SC-014**: **Canary survival is zero**: 100% of canary tokens seeded before each campaign are
  absent from every committed artifact, 100% of committed artifacts pass an independent
  secrets-and-entropy scan, zero people identifiers appear in any committed artifact in any form
  including hashed, and zero unsanitised payloads reach disk at any point including aborted runs.
- **SC-015**: The graph built live against the organisation equals the graph built from the
  recording of the same run, byte for byte, as in feature 001's live-run parity check.
- **SC-016**: Starting from an alert instant alone, Sam obtains the in-scope changes in the
  preceding window with, for each, the actor, the actor kind, the commit it shipped, whether it
  was a rollback and a link that opens the run — in one command and under thirty seconds on the
  recorded corpus.
- **SC-017**: Zero automated merges result from probable rules; every automated merge involving a
  deploy claim is explainable by the audit query in 100% of cases; and at least 95% of rollouts
  observed by both a deploy feeder and a platform feeder that share a commit or artefact
  identifier merge automatically under a certain rule.
- **SC-018**: 100% of changes whose targets cannot be resolved are kept as unattached rather than
  dropped, and attach automatically within one polling interval of the target appearing.

## Assumptions

- The organisation ships from GitHub Actions and hosts its front end on Vercel, and the two
  platforms shipped two of the three deployment-caused incidents in the September 2026 coverage
  audit. Feature 003 covers the third platform, Cloud Run, on the same mapping.
- Feature 001's feeder contract, event schema, resolution rules, pointer schema, fixture format
  and conformance suite are used unchanged. Anything these feeders need that they lack is a schema
  change with its own decision record, not a local extension; the list is **Dependencies on
  feature 001**.
- A deployment is a **change**, not an entity. Repositories, projects, runs and releases are
  claims, properties and links, so the graph does not acquire a parallel code-organisation
  taxonomy (FR-030). This is what keeps the feeders thin.
- The unit of a rollout change is (origin, target), and a monorepo therefore produces several
  changes (FR-017). The alternative — one change with several targets — was rejected because it
  cannot be merged pairwise against the platform's per-service observation of the same rollout.
- `deploy.commit_sha`, `deploy.image` and `deploy.release` are the whole cross-source vocabulary
  for this feature. If a platform feeder in 001 or 003 mints these keys with a different value
  form, the published normalisation (FR-046) is the fix, not a private adjustment in one feeder.
- Actor kind is derivable often, not always. Both platforms state enough to type the common cases;
  where they do not, `unknown` with the available evidence is the answer, and the consumer degrades
  rather than guesses.
- Rollback is only trustworthy where the platform states it. Vercel does; GitHub does not have a
  native rollback concept for deployments, so most GitHub rollbacks appear as ordinary rollouts of
  an earlier commit, and the spec accepts that rather than inferring (FR-016, SC-009).
- The environment filter is configuration with a published default, and its exclusions are counted
  rather than silent, because "nothing deployed" and "we were not looking" are different answers.
- The webhook is optional everywhere. Nothing in the design depends on it existing, and nothing
  reads its body; a deployment where no inbound endpoint can be exposed loses latency (SC-003) and
  nothing else.
- The recording campaign needs a private repository and private CI, a signing key and signatories,
  and a read-only credential from the platform owner before it can start.
- Executing the pointers these feeders write is out of scope. A `LOG` pointer at a run's job logs
  is a place to look; whether anything ever runs it is a later decision.

## Dependencies

- **Feature 001**: the event schema, the feeder contract, the published resolution rules, the
  pointer schema, the fixture format and the conformance suite; and the platform-observed rollouts
  (Kubernetes) that SC-004 merges against.
- **Feature 003**: the Cloud Run rollouts that this feature's claims must merge with. The claim
  keys in FR-041 and their normalisation (FR-046) MUST be agreed with 003 and published once, not
  twice.
- **Feature 002**: the consumer. Actor kind (FR-013), the rollback flag (FR-016) and the origin
  link (FR-014) exist because the investigation engine's causal ordering and its timeline need
  them.
- **The organisation**: a read-only GitHub App installation or fine-grained token and a read-only
  Vercel token (FR-002); the environment and deploy-workflow allowlists (FR-023, FR-024); the
  repository↔service mapping (FR-017); a private repository and private CI for the corpus
  (FR-060); and the signing key and signatories for the sanitisation manifests (FR-064).

## Dependencies on feature 001

Everything below is a **schema or contract change in feature 001**, not a local extension here
(constitution IX). Each is stated as what these feeders need and what they do until it exists.

- **An actor kind on a change.** A change carries an actor as a free-text name and nothing that
  says whether the actor was a person, CI or the platform (FR-013). This is the same dependency
  features 003 and 005 declare, and it is satisfied once, in ADR-0005 D1: the published set
  `{PERSON, AUTOMATION, CONTROLLER, VENDOR, UNKNOWN}` on the change node, on the ranked item and in
  the change-observation event. Until it exists, the feeders record the actor and hold the kind in a
  declared property, and feature 002's causal ordering cannot exonerate a `CONTROLLER` change.
- **A registry of claim namespaces, with `deploy.commit_sha`, `deploy.image` and `deploy.release`
  in it.** The published namespace list is Kubernetes- and OpenTelemetry-shaped. The three
  cross-source deploy claims (FR-041), plus `github.repo`, `github.change`, `vercel.project` and
  `vercel.change`, need registering **with their value normalisation** (FR-046), because their
  whole purpose is that two different feeders mint byte-identical values. A namespace registry
  that does not fix the value form does not deliver "one rollout, not two".
- **A certain rule for shared deploy identifiers.** The published certain rules compare service
  names and Kubernetes workloads. None of them says "two change observations that state the same
  `deploy.commit_sha` for the same target, within the same environment, are one change" — which is
  the rule SC-004 depends on. It needs publishing with its score and its conditions, including
  what happens when the target agrees and the commit does not, and vice versa.
- **A rollback marker on a change.** The change taxonomy has kinds but nothing that says a rollout
  undid an earlier one (FR-016). Either a published boolean on the change with the restored
  deployment named, or a distinct change kind — a decision for 001, not for a connector. Until it
  exists the feeders record it as a declared property and story 5 degrades to an ordinary rollout.
- **A published convention for "unattached", and for automatic attachment.** Changes whose targets
  the graph does not yet know are kept and attached when the target appears (FR-018). The marker
  and the attachment rule belong to feature 001, not to each connector inventing its own. This is
  acute here: a service is routinely created in the graph by the platform feeder minutes after its
  first deploy.
- **Registry entries for the pointer vocabularies.** A pointer's vocabulary is a free string. The
  GitHub and Vercel resource-link vocabularies (FR-049) need registered, versioned names and the
  published statement of why their selectors cannot be expressed in OpenTelemetry semantic
  conventions.
- **Nothing else.** The node taxonomy, the change kinds ROLLOUT and CONFIG_CHANGE, the bitemporal
  model, the identity-claim and resolution events, the `changed-by` edge and the checkpoint's gap
  declaration are sufficient as published.

## Out of scope for this feature

- **Any write to GitHub or Vercel**, of any kind: dispatching, re-running or cancelling a workflow,
  creating or updating a deployment or its status, creating or editing a release, promoting,
  rolling back, redeploying or deleting a deployment, changing an environment variable or a project
  setting, or writing a comment, check or commit status. There is no configuration that enables
  one.
- **Reading any secret value**, including GitHub Actions secrets and variables and Vercel
  environment variable values in plaintext or ciphertext.
- **CI test results, flaky-test tracking and build performance.** A failed test run is not a change
  to the production system.
- **Pull requests, reviews, review comments and code diffs.** The graph is a model of the running
  system, not a code index; a commit is a claim on a change, never a node with its contents.
- **Repositories, branches, issues, projects and teams as graph entities.** They are claims and
  properties (FR-030).
- **Executing the pointers these feeders write.** No telemetry backend is delivered here; running a
  log pointer is feature 003's and feature 005's contribution and a later decision for these
  platforms.
- **GitLab, Bitbucket, CircleCI, Jenkins, Argo and every other CI or hosting platform.** Later
  features on the same mapping; the point of publishing the deploy claim keys now is that they
  arrive without a schema change.
- **Cloud Run and Cloud Build** (feature 003) and **Datadog's view of deployments** (feature 005).
  Where they observe the same rollout, this feature's job is to make the merge possible, not to
  perform it.
- **Vercel analytics, web vitals, speed insights, edge config and firewall rules**; **GitHub
  Advanced Security, Dependabot and the dependency graph.** Reading them is a later increment, not
  a variant of this one.
- Inferring a rollback, a deployment target or an actor kind from a string when the platform does
  not state it.
- Any language-model reasoning. These feeders supply structure; they do not reason.
- Autonomous remediation, proposal or otherwise.
