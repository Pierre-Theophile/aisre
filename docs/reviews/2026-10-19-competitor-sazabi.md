# Competitive review: Sazabi (2026-10-19)

Researched 2026-10-19 from sazabi.com, docs.sazabi.com, the pricing page and seed-round coverage. Nothing below comes from using the product.

## What Sazabi is

- **Pitch:** "effortless observability": autonomous alerts, conversational debugging, and coding-agent integrations. They call themselves an "AI-native observability platform". They want to replace Datadog, and an add-on to it is not the goal ([sazabi.com](https://www.sazabi.com/)).
- **Data model:** logs first. Metrics, traces and webhook events are all normalized into logs, which go into one managed store that Sazabi hosts. Logs come in through OpenTelemetry, an SDK, or direct connections to CloudWatch, GCP and Vercel ([docs philosophy](https://docs.sazabi.com/get-started/philosophy)).
- **Loop:** detect, investigate, fix.
  - **Detect:** Sazabi watches the logs and opens "issues" without anyone writing monitors.
  - **Investigate:** each issue gets a background investigation against the GitHub code, run in a sandbox with CLIs.
  - **Fix:** the result is posted to Slack or Teams with the cause, and Sazabi can open PRs. In their alpha they ran 8,000 investigations, found 2,000 issues and opened 200 PRs.
- **Surfaces:**
  - multiplayer chat threads in place of dashboards;
  - Slack and Microsoft Teams;
  - a CLI, API, MCP server, TypeScript SDK and Terraform provider;
  - first-class use from Claude Code, Cursor and Codex.
- **Integrations:** a catalog of about 120 vendors. It includes Datadog, Grafana, Honeycomb, Sentry, PagerDuty, incident.io, Rootly, Linear, Jira, Notion, Argo CD, Kubernetes, Terraform, Postgres and many dev platforms ([integrations](https://docs.sazabi.com/catalogs/integrations)).
- **Deployment:** closed-source SaaS only, hosted in a US or EU region. No self-hosting or BYOC is mentioned. Their LLM vendors are not named ([data handling](https://docs.sazabi.com/security/data-handling)).
- **Pricing:** usage-based, not per user ([pricing](https://www.sazabi.com/pricing)). Every paid plan includes 200 GB ingested, 50 GB scanned, 20 issues and 500 AI credits.

  | Plan | Price per 30 days | Log retention |
  |---|---|---|
  | Hobby | $50 | 7 days |
  | Startup | $200 | 30 days |
  | Growth | $1,000 | 90 days |
  | Enterprise | custom | custom |

  Overage costs $0.10/GB ingested, $0.10/GB scanned, and **$4 per issue**.
- **Company:**
  - San Francisco, founded in 2025, YC Spring 2026.
  - $8M seed announced June 25, 2026, co-led by J2 Ventures, Village Global and YC, with more than 60 angels from Vercel, Cursor, OpenAI, Anthropic and others.
  - The CEO, Sherwood Callaway, was previously at Brex infrastructure and is a two-time YC founder.
  - In closed alpha they onboarded 50 teams in two weeks and were heading for a public launch ([Pulse 2.0](https://pulse2.com/sazabi-raises-8-million-seed-round-to-build-ai-native-observability-platform/), [Dealroom](https://dealroom.co/news/136065-sazabi-raises-8m-to-rethink-observability-for-the-ai-era/)).
- **Target customer:** small, fast-moving teams with many AI coding agents. Their typical customer is on Vercel or Supabase, has no SRE team and has no Datadog contract worth protecting.

## Where we overlap

- Both run an LLM investigator that answers "what broke and why" with evidence and the queries it ran.
- Both use Slack as the incident surface.
- Both read GitHub for code and change context.
- Both connect to GCP and Vercel.
- Both use Notion for knowledge.

## Where we differ

| | Sazabi | aisre |
|---|---|---|
| Category | Replaces observability (stores your logs) | Sits on top of the observability you already have (reads Datadog/GCP in place) |
| Substrate | Logs, with everything normalized into logs | Bitemporal event-sourced graph of topology, changes and owners |
| Main question | "What is wrong in these logs?" | "What did the system look like at 14:32, and what changed since 13:00?" |
| Trigger | Proactive: detects issues itself | Reactive: starts from an alert or incident |
| Actions | Opens PRs | Proposes and never acts (constitution) |
| Knowledge | Notion is one integration among ~120 | Postmortems are RAG-indexed and cited; there is a service catalog with corrections |
| Openness | Closed SaaS, their LLMs, their storage | Apache-2.0, every dependency pluggable, self-hostable |
| Buyer | Startups with no SRE and no Datadog | Orgs with an existing stack, data constraints, real topology |

## Where we can win

1. **Self-hosted and open.** Regulated buyers, EU buyers who want more than "EU region", and anyone who will not ship all their logs to a seed-stage vendor cannot buy Sazabi. For them, "your data stays in your Postgres, your LLM, your embedder" is the whole argument.
2. **No rip-and-replace.** Sazabi wins when you have no observability yet. We win when you already pay Datadog or GCP and want an agent on top. For that buyer, moving logs to a new store is a migration project, and it adds a second ingestion bill.
3. **Changes and topology.** A logs-first model can't easily answer "which deploy, config flip or dependency changed upstream of this" across time. That question is exactly what our graph's `diff` and `impact` do. Our eval (true cause = a graph node at alert time) is the evidence to publish.
4. **Organizational memory.** Postmortem RAG, the service catalog with human corrections, and auditable entity resolution compound over time. Sazabi's lighter, chat-first approach doesn't show this.
5. **Cost shape.** $4 per detected issue plus ingest and scan fees gets expensive at scale. Our costs are the user's own infrastructure and their own LLM.

## Where they are ahead

- **A live, funded product with paying users.** We have no live installation yet; our recording campaigns are waiting on credentials.
- **Proactive detection:** nobody has to write monitors.
- **The coding-agent loop:** MCP, CLI, and investigations that end in a PR. This is where the "AI-native team" market is heading.
- **Breadth of integrations and setup in minutes.** We have about ten feeders, and most of them are verified only on synthetic twins, waiting on a recording campaign.

## What it changes in the roadmap

Adopted as [ADR-0016](../decisions/ADR-0016-roadmap-after-the-sazabi-review.md):

1. **Positioning:** an open change-and-topology substrate plus an investigator, on top of the stack you run.
2. **Finish 009 first.**
3. **010:** a read-only MCP server for coding agents.
4. **011:** change-watch intake, where the graph starts investigations after risky changes.
5. **A live installation on the owner's estate** is a roadmap item.
6. **Setup in minutes**, after 010.
7. **Reaffirmed non-goals:** no log storage, no anomaly detection over raw logs, no autonomous PRs, no hosted service.

The Datadog connector (005) that this review first suggested pulling forward is already built (quickstart run 2026-09-27).
