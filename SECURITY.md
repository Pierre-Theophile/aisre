# Security Policy

## Supported versions

The project is pre-1.0 and under active development. Security fixes land on `main` and in the
next tagged release. Only the latest tag and `main` are supported; there are no backports to
earlier tags yet.

| Version | Supported |
|---------|-----------|
| `main`  | yes |
| latest tag | yes |
| anything older | no |

## Reporting a vulnerability

**Do not open a public issue for a security problem.**

Report privately through
[GitHub Security Advisories](https://github.com/Pierre-Theophile/aisre/security/advisories/new)
("Report a vulnerability" on the Security tab). If you cannot use that, email the maintainer
listed in `CODEOWNERS` and say "security" in the subject.

Please include:

- what an attacker can do, and what access they need to start;
- the affected version or commit;
- reproduction steps — a fixture, event stream or request that triggers it is ideal;
- any suggested mitigation.

## What to expect

| Stage | Target |
|-------|--------|
| Acknowledgement of your report | 3 working days |
| Initial assessment and severity | 10 working days |
| Fix or documented mitigation for critical issues | 30 days from acknowledgement |

Progress is shared in the private advisory thread. When a fix ships, the advisory is published
with a CVE where applicable, and you are credited unless you prefer otherwise. We ask that you
give us the 30 days above before disclosing publicly, and we will tell you promptly if a fix
will take longer.

## Scope

In scope: this repository's source, its published protobuf schema, its generated code, its
container images, its CI workflows, and the credentials handling of the reference feeders.

Particularly interesting to us:

- **Authentication and authorization**: any path that reads or writes the graph without a
  verified individual principal, or that lets a `reader` principal perform a `decider` action,
  or that lets a feeder token write outside its registered `source_id`.
- **History integrity**: any way to delete, truncate or silently rewrite the event log or a
  version row. History is append-only; corrections open a new version, they never overwrite
  one.
- **Secret leakage**: any path that persists a secret value into the graph. The Kubernetes
  feeder deliberately records Secret *versions* only, never values.
- **Telemetry leakage**: any path that persists metric samples, log lines or spans. The graph
  stores pointers to telemetry, not telemetry, and the validator rejects payloads that would.
- **Privilege escalation toward production**: the tool is read-only toward every system it
  observes. Any code path that obtains or requires write scope is a vulnerability, not a
  feature.
- **Injection** into SQL, into pointer selectors that are later executed against a telemetry
  backend, or into the OTLP receiver.

Out of scope: vulnerabilities in third-party dependencies with no exploitable path in this
project (report those upstream; `govulncheck` runs in CI and we will pick up the bump),
findings that require an already-compromised database or host, denial of service from
unbounded self-hosted load, missing hardening headers on the local development server, and
social engineering.

## Operating sre-agent safely

- Give every connector **read-only credentials**. The Kubernetes feeder refuses to start if it
  is granted a write verb on a resource it watches.
- Put the API behind your identity provider. Individual OIDC identity is required on every
  call; the `--auth=dev` provider is for fixtures, CI and laptops only, refuses to start
  without `--dev`, and prints a banner when it does.
- Treat the graph database as sensitive: it is a map of your production topology, its owners
  and its change history. Restrict network access and back it up like any other system of
  record.
- Pointers are query expressions, not data. Review any pointer before executing it against a
  telemetry backend with credentials of your own.
