# Literature review: agentic root-cause analysis and topology discovery (2026-11-01)

Researched 2026-11-01 on arXiv (through alphaXiv), at the owner's request: "check whether there are
scientific advances that let us do better than competitors here; if so, we integrate them
directly". Most sources are 2026 preprints evaluated on injected faults in demo systems (OpenTelemetry
Demo, TrainTicket, Online Boutique), almost all on Kubernetes. Trends are robust; absolute numbers are
not, and none of them was measured on our substrate.

The decisions this review leads to are in
[ADR-0019](../decisions/ADR-0019-roadmap-after-the-rca-literature-review.md).

## Findings

### F1. Do not prescribe the investigation

*Free to Look, Free to Reason* (Bi et al., Peking University, arXiv:2610.35767, October 2026) runs
three simple harnesses, five published RCA agents and seven statistical or causal methods on the same
90 validated incidents pooled from eight public datasets (RCAtlas), across six models.

- The simple harnesses prescribe none of the model's calls: one call over an evidence package, a SQL
  agent over the raw telemetry, and Codex. They reach 65–77 % top-1 on mid-tier models.
- The published agents fix part of the investigation in the program: standard operating procedures,
  fixed stages, role agents, votes. They reach 16–53 %. Non-LLM methods reach 6–39 %.
- The mechanism, shown on one case and measured as a "prescribed-call ratio" on all 90: when the
  program decides what is read, it skips the decisive evidence. In the case study, the procedure FoA
  matched had no log step, so it never read the "connection refused" line; mABC walked the call chain
  down from the alert and never reached a service that had *stopped receiving* calls.
- A stronger model mainly improves reliability. More reasoning effort changes accuracy by a few
  points while multiplying time by up to seven.
- The authors' own conclusion: domain knowledge is not useless, but it should be offered to the model
  as optional guidance, not fixed into the control flow.

### F2. The leverage is a harness that learns the system, behind a regression gate

*OpsHarness* (Huang et al., CUHK and ByteDance, arXiv:2608.25661, August 2026) wraps a general agent
(Codex, Claude Code) instead of building an RCA agent from scratch.

- At setup it writes a **system profile** automatically: schema, data layout, component inventory. It
  is read in full at the start of every diagnosis, and it replaces a per-dataset adapter.
- After each diagnosis a feedback hook asks for a thumbs-up or thumbs-down and the actual root cause.
  An evolver mines correct and incorrect trajectories into atomic proposals (workflow skeletons,
  operation skills, positive and negative rules, tool cards).
- A proposal is admitted only through a **dual gate**: strictly better on the cases it was mined from,
  and no regression on a disjoint held-out set.
- Results: 59.0 % top-1 against 36.1 % for the bare agent, averaged over four backbones; the
  final-window accuracy reaches 0.83 against 0.43 without evolution. **Without the gate, accuracy
  decays to 0.33, below the agent that does not learn at all**, because weak proposals overfit.

### F3. Correct localisation hides a broken causal story, and failures fall into three families

*Beyond Fault Localization* (Lu et al., CUHK-Shenzhen, arXiv:2608.21310, 5,000 trajectories, nine
frameworks, two backbones) evaluates the process, against annotated propagation paths.

- Edge F1 (reconstructing how the fault propagated) trails node F1 (which services were involved) in
  every configuration, even on correctly localised cases; its ceiling stays near 0.67.
- Accuracy falls as the causal chain from root cause to alerting service lengthens.
- Successful runs stay on the fault-impact surface and act on the evidence they retrieve. Failed runs
  stray to unrelated services, ignore decisive evidence, or loop on shallow queries.
- Failures fall into three families: **omission** (decisive evidence reachable but never queried),
  **misread** (retrieved but misinterpreted or scoped to the wrong dependency), and **ungrounded
  reasoning**.
- *DiagGuard*, two answer-agnostic defences mined from these modes, improves top-1 in all fourteen
  tested configurations by 3.5 to 35.1 points: a **Grounder** that hands the agent a profile of the
  search space before its first query, and a **Verifier** that runs a self-audit before the
  investigation and a soundness check before the answer is committed.
- OpenRCA 2.0 (arXiv:2606.27154, cited there) reports the same gap: 76 % of agents name a correct
  root-cause service, 61.5 % follow a verified propagation path.

The owner reported the omission mode on his own estate (2026-11-01): an agent connected to GCP Cloud
Logging and Monitoring proposed a cause without ever reading the metrics that mattered for that kind
of incident, and nothing in its answer said so.

### F4. Graph structure helps as a map for the agent, not as a model feature

*Does Graph Structure Earn Its Place in Microservice RCA?* (Buljić, arXiv:2609.27069) isolates the
neighbour term of a graph encoder on RCAEval.

- No reliable effect of graph structure on localisation, in-distribution or across systems.
- RCAEval injects faults into five services per system: a ranker that reads no telemetry scores
  Avg@5 0.488 in-distribution, and that prior collapses to 0.192 across systems. Benchmark priors can
  masquerade as method quality.

This does not contradict the substrate thesis: the graph's value here is scoping, dating and
navigating an investigation (F3's Grounder), and finding *changes*, which no flat model does. It does
warn against claiming accuracy from the graph without an evaluation that controls for priors.

### F5. Entities are heterogeneous, propagation is asymmetric, instances churn

*Which Types of Heterogeneity Matter for Root Cause Localization?* (Wang et al., Nankai and Tsinghua,
arXiv:2604.26670) on two industrial datasets:

- Most fault cases propagate across entity layers (service ↔ host), and propagation is asymmetric:
  service-level faults often reach their hosts, the reverse is rare.
- Modelling services and hosts as distinct typed nodes with typed edges is the component whose
  removal costs most accuracy.
- Events are abstracted at service level, not at volatile instance level, to survive autoscaling and
  churn.

### F6. Serverless needs the platform's lifecycle, not only the application's

*FaaSRCA* (Huang et al., Sun Yat-sen, arXiv:2412.02239):

- Serverless instances live hundreds of milliseconds to minutes and emit pulse-like, discontinuous
  data; microservice RCA methods built on continuous time series degrade on it.
- A large class of faults happens on the platform side (instance creation, image pull, scheduling)
  and is invisible in application telemetry.
- A global call graph that joins platform-side lifecycle events to application-side calls reaches
  92.5 % HR@k against 73.3 % for the best microservice baseline.

### F7. Topology discovery: traces suffice for a connectivity model; networks lie across NAT

- *Trace-discovered resilience models* (Krasnovsky, arXiv:2512.12314; and arXiv:2506.11176, ICSE
  2026): a connectivity-only service graph mined from OpenTelemetry traces reproduces the end-to-end
  availability curve under fail-stop faults. Treating Kafka edges as asynchronous changed predicted
  availability by about 10⁻⁵ for immediate HTTP success. Async edges should be typed, but they matter
  only when success is defined on eventual processing.
- *Ripple* (Landau et al., Utrecht and Porto, arXiv:2510.15490): passive network observation
  (Caretta, Coroot) misattributes or misses dependencies across NAT, container NAT and cloud
  gateways. Ripple injects an identifier in a TCP option with eBPF and resolves the receiving
  process; precision and recall are 100 % in most scenarios. It needs an agent on every host, so it
  cannot run on managed serverless platforms.

### F8. GPU inference is its own stack

*TELLER* (Xu et al., Sun Yat-sen, ASE 2026, arXiv:2608.01975): non-intrusive NVTX/CUPTI tracing of
LLM inference reconstructs per-request call chains across the engine (vLLM, SGLang), the framework,
CUDA runtime and driver calls, kernels and NCCL collectives, and aligns them with service logs. Not
actionable before a GPU-workload connector exists; recorded as a later differentiator.

## What we already do that the literature validates

- One investigator owning one belief state, with workers bound to sources of truth (002, ADR-0003):
  consistent with F1, provided the workers stay tools the investigator chooses to call.
- Evidence chains, calibrated confidence and `unknown` (constitution V): the precondition of F3's
  Verifier.
- Replayable fixtures that fail CI on regression (constitution VIII): exactly F2's outer gate.
- The bitemporal graph: a lost edge or a stopped flow is a closed valid interval, which is the
  signal F1's failing agents never saw.
- No dedicated graph database without a benchmark (constitution X): consistent with F4.

## Sources

- [Free to Look, Free to Reason: Simple LLM Harnesses Outdiagnose Hand-Crafted RCA Agents](https://arxiv.org/abs/2610.35767)
- [From General Agents to RCA Experts: A Self-Evolving Harness for Root Cause Analysis](https://arxiv.org/abs/2608.25661)
- [Beyond Fault Localization: A Trajectory-Level Study of LLM Agents for Microservice RCA](https://arxiv.org/abs/2608.21310)
- [OpenRCA 2.0: From Outcome Labels to Causal Process Supervision](https://arxiv.org/abs/2606.27154)
- [Does Graph Structure Earn Its Place in Microservice Root-Cause Analysis?](https://arxiv.org/abs/2609.27069)
- [Which Types of Heterogeneity Matter for Root Cause Localization in Microservice Systems?](https://arxiv.org/abs/2604.26670)
- [FaaSRCA: Full Lifecycle Root Cause Analysis for Serverless Applications](https://arxiv.org/abs/2412.02239)
- [Evaluating Asynchronous Semantics in Trace-Discovered Resilience Models](https://arxiv.org/abs/2512.12314)
- [Model Discovery and Graph Simulation: A Lightweight Gateway to Chaos Engineering](https://arxiv.org/abs/2506.11176)
- [Seeing Through NAT with Ripple: Retrofitting Service Dependency Discovery](https://arxiv.org/abs/2510.15490)
- [TELLER: Non-intrusive Cross-Layer Root-Cause Analysis for LLM Inference](https://arxiv.org/abs/2608.01975)
