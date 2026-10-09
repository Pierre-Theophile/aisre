# What the 003 and 004 recording campaigns need from the organisation

**Unblocks**: 003 T156, T157, then T172, T173 · 004 T114, T115, T116, then T112, T125–T127, and
T034 with a project-scoped Vercel token · **Check with**: `scripts/campaign-preflight.sh`

Every open task in features 003 and 004 waits on something only the organisation can supply. This
page lists each item, who can grant it, exactly what to grant, and the environment variable the
campaign reads it from. Feature 005 was unblocked the same way on 2026-09-29: a read-only key in
the environment and a named signatory.

Secrets go in the cloud environment's settings, under Network secrets or as environment
variables, and never in a chat, a commit or a command line. A new session picks them up. Then
`scripts/campaign-preflight.sh` reports what is still missing, and runs each connector's read-only
gate on what is there.

> **Start recording as soon as the credentials exist, before the scope is settled.** Topology
> history and an announcement stream cannot be reconstructed after the fact (003 FR-130). A scope
> change mid-campaign is one `fixture campaign record --add-scope` call; a late start is permanent.

---

## People, which no credential replaces

| who | does what | variable | unblocks |
|---|---|---|---|
| **a named signatory**, and a second where a second person exists | signs the sanitised corpus manifest (003 FR-140, 004 FR-064). `fixture campaign verify` refuses a recording without a signed, current manifest | `SRE_AGENT_SIGNATORY` (the name, recorded in the campaign record) | 003 T157, 004 T115 |
| **the notice mailbox's owner** | authorises the read of one mailbox, by name and date (003 FR-132) | recorded as `--authorised-by` / `--authorised-on` | 003 T156 |

## Both campaigns

| item | variable | notes |
|---|---|---|
| organisation slug | `SRE_AGENT_ORG` | every source id carries it (`gcp:<org>`, `github:<org>`) |
| corpus HMAC key | `SRE_AGENT_CORPUS_KEY` | the connectors sanitise with it before anything touches disk (FR-137). Generate once, for example with `openssl rand -hex 32`, and keep it in the secret store: the same key must sanitise every recording in the corpus |

## 003: GCP

A service account holding only the roles of [gcp.md §1](../connectors/gcp.md), on each project in
scope. **Not** `roles/cloudsql.viewer`, which grants `cloudsql.instances.export`; **not**
`roles/viewer` or an owner account. The gate refuses those, naming the offending permission.

```bash
P=<project>
gcloud iam service-accounts create sre-agent-reader --project=$P
SA=sre-agent-reader@$P.iam.gserviceaccount.com
# the custom Cloud SQL role, file content in docs/connectors/gcp.md §1
gcloud iam roles create sreAgentCloudSqlReader --project=$P --file=sre-agent-cloudsql-reader.yaml
for r in roles/run.viewer roles/logging.viewer roles/monitoring.viewer \
         roles/container.clusterViewer projects/$P/roles/sreAgentCloudSqlReader; do
  gcloud projects add-iam-policy-binding $P --member=serviceAccount:$SA --role=$r
done
# P3 areas, only if load balancers and DNS are in scope:
#   roles/compute.viewer (not compute.networkViewer, which writes) and roles/dns.reader
```

| variable | value |
|---|---|
| `GCP_SERVICE_ACCOUNT_JSON` | the service account key's JSON contents, or set `GOOGLE_APPLICATION_CREDENTIALS` to a path where the session has one |
| `GCP_PROJECTS` | comma-separated project ids in scope |
| `GCP_REGIONS` | comma-separated regions in scope |

Where the organisation's policy forbids service account keys
(`iam.disableServiceAccountKeyCreation`), run the campaign from a machine of the organisation's
with Application Default Credentials impersonating the account instead. The commands are the same.

## 003: the vendor-notice mailbox

The connector reads one mailbox per entry in `config/vendors.yaml`, which ships with none: the
address is the organisation's to choose. Before choosing, read
[vendor-notice.md §3–§4](../connectors/vendor-notice.md):

- **Not a Google Group.** A Group's archive is reachable by no API, so the connector would read
  nothing, forever, without erroring. Use a subscribed member's mailbox.
- **Notices go to each project's Essential Contacts**, not to a shared address unless somebody
  configured it as a contact. Check that the chosen mailbox actually receives them.
- **Prefer IMAP with a per-mailbox credential** (`imap_examine_peek`). Gmail API read-only access
  through domain-wide delegation is a tenant-wide grant: it can read *any* user's mail.

The entry names the variable holding the credential (`password_env` for IMAP, `token_env` for the
Gmail API), and that variable goes in the environment. A campaign whose mailbox yields nothing fails
at campaign start (FR-132b) rather than recording silence.

## 004: GitHub

A **GitHub App** installed on the organisation, not a personal access token. Its installation's
repository selection is the boundary the platform enforces ([github.md §1](../connectors/github.md)).

| permission | access |
|---|---|
| Deployments | read-only |
| Actions | read-only |
| Contents | read-only |
| Metadata | read-only (mandatory) |

Nothing else, at any level: no Secrets, Variables, Issues, Pull requests, Members or Administration.
The gate refuses any write permission; it does not refuse an extra *read*, so a wider grant is the
installer's to avoid.

| variable | value |
|---|---|
| `GITHUB_APP_ID` | the App's client ID (`Iv23…`) |
| `GITHUB_APP_PRIVATE_KEY` | the App's PEM private key, the whole file's contents |
| `GITHUB_INSTALLATION_ID` | the installation's numeric id, from its settings page URL |

The connector never reads the key. `scripts/github-app-jwt.sh` does, at each token renewal, as the
`--app-assertion-command` helper of [github.md §7.2](../connectors/github.md). Where a KMS or a vault
can sign the JWT instead, a helper that asks it to is the better shape, because then the key never
enters the session at all.

## 004: Vercel

A Vercel access token. **Create it with a `projectId` where possible.** A project-scoped token is a
boundary the platform enforces; an unscoped one is only narrowed by configuration
([vercel.md §1](../connectors/vercel.md)). A project-scoped token is also the only way to close
T034: the spelling of a project scope in `token.scopes[].type` is not documented, and somebody has to
read a real one back.

| variable | value |
|---|---|
| `VERCEL_TOKEN` | the access token |
| `VERCEL_TEAM` | the team id, where the token is a personal one |

Vercel reports nothing about whether a token can write, so the live run takes the operator's
`--assert-read-only` and the checkpoint records it as `operator_asserted`. Whoever creates the token
is the right person to make that statement.

---

## Then

1. `scripts/campaign-preflight.sh` until it reports `ready`.
2. Quickstart §7 then §8 of [003](../../specs/003-gcp-integration/quickstart.md) and
   [004](../../specs/004-deploy-feeders/quickstart.md): `fixture campaign record` first, with the
   signatory and the mailbox authorisation, then the live feeds with `--record`, then sanitise,
   scan, sign, verify and parity.
3. The verification tasks that wait on a recorded corpus (003 T172, T173; 004 T125–T127) follow from
   the committed corpus.
