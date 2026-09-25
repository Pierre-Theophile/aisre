// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The benchmark's answer (T091, research §2, ADR-0002).
//
// A benchmark report is evidence in an architecture decision, so this one states its verdict
// rather than leaving a reader to compare numbers against a document. Three thresholds are
// checked and named:
//
//	SC-001          2-hop subgraph as-of, p95 < 1 s
//	SC-002          24 h diff with ranking, p95 < 2 s
//	ADR-0002 exit   the same two at 2×, plus a full replay under 60 min
//
// The first two are the product promise. The third is the line that would reopen the storage
// decision (constitution X): missing it is not a performance bug to be tuned away quietly, it
// is a finding that says "evaluate Apache AGE", and it is written into the report in those
// words.

// BenchReport is one benchmark run, rendered to Markdown for `docs/benchmarks/` and to JSON for
// a trend line.
type BenchReport struct {
	Scale         string           `json:"scale"`
	Seed          int64            `json:"seed"`
	Queries       int              `json:"queries_per_family"`
	StartedAt     time.Time        `json:"started_at"`
	FinishedAt    time.Time        `json:"finished_at"`
	Environment   BenchEnvironment `json:"environment"`
	Workload      BenchWorkload    `json:"workload"`
	Graph         BenchGraphCounts `json:"graph"`
	Ingest        BenchIngest      `json:"ingest"`
	Replay        BenchReplay      `json:"replay"`
	Latencies     []BenchLatency   `json:"latencies"`
	DatabaseBytes int64            `json:"database_bytes"`
	Criteria      []BenchCriterion `json:"criteria"`
	Passed        bool             `json:"passed"`
}

// BenchEnvironment is what the numbers were measured on. A benchmark without it is a number
// without units.
type BenchEnvironment struct {
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	CPU             string `json:"cpu"`
	CPUCount        int    `json:"cpu_count"`
	MemoryBytes     int64  `json:"memory_bytes"`
	GoVersion       string `json:"go_version"`
	PostgresVersion string `json:"postgres_version"`
	PostgresServer  string `json:"postgres_server"`
	Note            string `json:"note,omitempty"`
}

// BenchWorkload is what was asked for.
type BenchWorkload struct {
	Entities int `json:"entities"`
	Edges    int `json:"edges"`
	Changes  int `json:"changes"`
	Claims   int `json:"claims"`
	Events   int `json:"events"`
	SpanDays int `json:"span_days"`
}

// BenchGraphCounts is what the graph ended up holding.
type BenchGraphCounts struct {
	Entities       int `json:"entities"`
	EntityVersions int `json:"entity_versions"`
	EdgeVersions   int `json:"edge_versions"`
	Claims         int `json:"identity_claims"`
	Merges         int `json:"merged_entities"`
	Suggestions    int `json:"suggestions"`
	LoggedEvents   int `json:"logged_events"`
}

// BenchIngest is the ingestion measurement.
type BenchIngest struct {
	Events    int     `json:"events"`
	Seconds   float64 `json:"seconds"`
	PerSecond float64 `json:"events_per_second"`
}

// BenchReplay is the replay measurement.
type BenchReplay struct {
	Measured  bool    `json:"measured"`
	Events    int     `json:"events"`
	Seconds   float64 `json:"seconds"`
	PerSecond float64 `json:"events_per_second"`
	// BatchSize is how many events shared one transaction. It belongs in the report because
	// it is the number the 2026-09-16 replay finding was about: the same events replayed one
	// transaction at a time and 500 at a time are two different measurements, and a report
	// that does not say which one it is cannot be compared with the next one.
	BatchSize int `json:"batch_size"`
}

// BenchLatency is one query family's percentiles, in milliseconds.
type BenchLatency struct {
	Name   string  `json:"name"`
	Detail string  `json:"detail"`
	Count  int     `json:"count"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MaxMs  float64 `json:"max_ms"`
}

// BenchCriterion is one published threshold and whether this run met it.
type BenchCriterion struct {
	ID      string `json:"id"`
	What    string `json:"what"`
	Target  string `json:"target"`
	Actual  string `json:"actual"`
	Passed  bool   `json:"passed"`
	Gate    bool   `json:"gate"`
	Skipped bool   `json:"skipped,omitempty"`
}

// Query family names, used as the row labels of the latency table and as the keys the criteria
// look themselves up by.
const (
	BenchQuerySubgraph2 = "subgraph-2hop-asof"
	BenchQuerySubgraph3 = "subgraph-3hop-capped"
	BenchQueryDiff1h    = "diff-1h"
	BenchQueryDiff24h   = "diff-24h"
	BenchQueryDiff7d    = "diff-7d"
	BenchQueryImpact    = "impact"
)

// evaluate fills in Criteria and Passed.
//
// replayOnly says the run measured the replay and nothing else, so the query and ingestion
// criteria are reported as not measured rather than as met by a zero. A skipped gate never
// fails the run — it is a deliberate choice — but it is always named, here and in the headline.
func (r *BenchReport) evaluate(skippedReplay, replayOnly bool) {
	slices.SortStableFunc(r.Latencies, byName)
	twoHop := r.latency(BenchQuerySubgraph2)
	diff24h := r.latency(BenchQueryDiff24h)

	if replayOnly {
		r.Criteria = []BenchCriterion{
			notMeasured("SC-001", "subgraph 2-hop as-of, p95", "< 1.00 s", true),
			notMeasured("SC-002", "diff over 24 h with ranking, p95", "< 2.00 s", true),
			notMeasured("ADR-0002", "subgraph 2-hop as-of, p95 (2× exit criterion)", "< 2.00 s", true),
			notMeasured("ADR-0002", "diff over 24 h, p95 (2× exit criterion)", "< 4.00 s", true),
		}
	} else {
		r.Criteria = []BenchCriterion{
			threshold("SC-001", "subgraph 2-hop as-of, p95", 1000, twoHop.P95Ms, true),
			threshold("SC-002", "diff over 24 h with ranking, p95", 2000, diff24h.P95Ms, true),
			threshold("ADR-0002", "subgraph 2-hop as-of, p95 (2× exit criterion)", 2000, twoHop.P95Ms, true),
			threshold("ADR-0002", "diff over 24 h, p95 (2× exit criterion)", 4000, diff24h.P95Ms, true),
		}
	}

	replay := BenchCriterion{
		ID: "ADR-0002", What: "full replay of the event log", Target: "< 60 min", Gate: true,
	}
	switch {
	case skippedReplay || !r.Replay.Measured:
		replay.Skipped = true
		replay.Actual = "not measured"
		replay.Passed = true
	default:
		replay.Actual = roundDur(time.Duration(r.Replay.Seconds * float64(time.Second))).String()
		replay.Passed = r.Replay.Seconds < 60*60
	}
	r.Criteria = append(r.Criteria, replay)

	// plan.md's performance goals are reported but do not gate the storage decision.
	if replayOnly {
		r.Criteria = append(r.Criteria,
			notMeasured("plan", "sustained ingestion", "≥ 500 events/s", false))
	} else {
		r.Criteria = append(r.Criteria,
			BenchCriterion{
				ID: "plan", What: "sustained ingestion", Target: "≥ 500 events/s",
				Actual: fmt.Sprintf("%.0f events/s", r.Ingest.PerSecond),
				Passed: r.Ingest.PerSecond >= 500,
			},
		)
	}
	if r.Replay.Measured {
		r.Criteria = append(r.Criteria, BenchCriterion{
			ID: "plan", What: "replay of the event log", Target: "< 30 min",
			Actual: roundDur(time.Duration(r.Replay.Seconds * float64(time.Second))).String(),
			Passed: r.Replay.Seconds < 30*60,
		})
	}

	r.Passed = true
	for _, c := range r.Criteria {
		if c.Gate && !c.Passed {
			r.Passed = false
		}
	}
}

// notMeasured is a criterion this run deliberately did not measure. It passes — skipping is a
// choice, not a regression — and is counted by skippedGates so the headline says so.
func notMeasured(id, what, target string, gate bool) BenchCriterion {
	return BenchCriterion{
		ID: id, What: what, Target: target, Actual: "not measured",
		Passed: true, Gate: gate, Skipped: true,
	}
}

func threshold(id, what string, targetMs, actualMs float64, gate bool) BenchCriterion {
	return BenchCriterion{
		ID: id, What: what,
		Target: fmt.Sprintf("< %s", msString(targetMs)),
		Actual: msString(actualMs),
		Passed: actualMs > 0 && actualMs < targetMs,
		Gate:   gate,
	}
}

func msString(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.2f s", v/1000)
	}
	return fmt.Sprintf("%.0f ms", v)
}

// skippedGates counts the gating criteria this run did not measure.
func (r *BenchReport) skippedGates() int {
	n := 0
	for _, c := range r.Criteria {
		if c.Gate && c.Skipped {
			n++
		}
	}
	return n
}

func (r *BenchReport) latency(name string) BenchLatency {
	for _, l := range r.Latencies {
		if l.Name == name {
			return l
		}
	}
	return BenchLatency{Name: name}
}

// FileStem is the basename the report is written under, without an extension:
// `<date>-scale-<scale>`.
func (r *BenchReport) FileStem() string {
	return fmt.Sprintf("%s-scale-%s", r.StartedAt.Format("2006-01-02"), r.Scale)
}

// JSON renders the machine-readable form.
func (r *BenchReport) JSON() ([]byte, error) {
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("fixture: encode benchmark report: %w", err)
	}
	return append(encoded, '\n'), nil
}

// Markdown renders the committed report.
func (r *BenchReport) Markdown() string {
	var b strings.Builder
	// The repository's documentation convention (research §17): every committed file carries
	// the licence identifier, generated ones included.
	b.WriteString("<!-- SPDX-License-Identifier: Apache-2.0 -->\n\n")
	fmt.Fprintf(&b, "# Reference workload benchmark — scale %s\n\n", r.Scale)
	fmt.Fprintf(&b, "%s · seed %d · %d queries per family · duration %s\n\n",
		r.StartedAt.Format(time.RFC3339), r.Seed, r.Queries,
		roundDur(r.FinishedAt.Sub(r.StartedAt)))

	// A skipped gate is not a passed gate. A run that leaves one out still says PASS — skipping
	// is a deliberate choice, not a regression, and failing the exit code for it would make
	// `--skip-replay` useless — but the headline has to say what was not measured, or the
	// report claims evidence it does not have.
	verdict := "**FAIL** — at least one gate was missed; see Criteria."
	switch {
	case r.Passed && r.skippedGates() > 0:
		gates := "gate was"
		if r.skippedGates() > 1 {
			gates = "gates were"
		}
		verdict = fmt.Sprintf(
			"**PASS, INCOMPLETE** — every criterion this run measured is met, but %d %s not measured; see Criteria.",
			r.skippedGates(), gates)
	case r.Passed:
		verdict = "**PASS** — SC-001, SC-002 and the ADR-0002 exit criteria are all met."
	}
	fmt.Fprintf(&b, "%s\n\n", verdict)

	b.WriteString("## Environment\n\n")
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| CPU | %s (%d logical) |\n", orUnknown(r.Environment.CPU), r.Environment.CPUCount)
	fmt.Fprintf(&b, "| Memory | %s |\n", humanBytes(r.Environment.MemoryBytes))
	fmt.Fprintf(&b, "| OS / arch | %s / %s |\n", r.Environment.OS, r.Environment.Arch)
	fmt.Fprintf(&b, "| Go | %s |\n", r.Environment.GoVersion)
	fmt.Fprintf(&b, "| PostgreSQL | %s |\n", orUnknown(r.Environment.PostgresVersion))
	fmt.Fprintf(&b, "| Postgres server | %s |\n", orUnknown(r.Environment.PostgresServer))
	if r.Environment.Note != "" {
		fmt.Fprintf(&b, "| Note | %s |\n", r.Environment.Note)
	}
	b.WriteString("\n")

	b.WriteString("## Workload\n\n")
	b.WriteString("| | requested | in the graph |\n|---|---:|---:|\n")
	fmt.Fprintf(&b, "| entities | %s | %s |\n", count(r.Workload.Entities), count(r.Graph.Entities))
	fmt.Fprintf(&b, "| edge versions | %s | %s |\n", count(r.Workload.Edges), count(r.Graph.EdgeVersions))
	fmt.Fprintf(&b, "| entity versions | — | %s |\n", count(r.Graph.EntityVersions))
	fmt.Fprintf(&b, "| change nodes | %s | — |\n", count(r.Workload.Changes))
	fmt.Fprintf(&b, "| identity claims | %s | %s |\n", count(r.Workload.Claims), count(r.Graph.Claims))
	fmt.Fprintf(&b, "| merged entities | — | %s |\n", count(r.Graph.Merges))
	fmt.Fprintf(&b, "| suggestions | — | %s |\n", count(r.Graph.Suggestions))
	fmt.Fprintf(&b, "| events | %s | %s |\n", count(r.Workload.Events), count(r.Graph.LoggedEvents))
	fmt.Fprintf(&b, "| valid-time span | %d days | — |\n", r.Workload.SpanDays)
	fmt.Fprintf(&b, "| database size | — | %s |\n", humanBytes(r.DatabaseBytes))
	b.WriteString("\n")

	b.WriteString("## Throughput\n\n")
	b.WriteString("| | events | wall time | events/s |\n|---|---:|---:|---:|\n")
	if r.Ingest.Events > 0 {
		fmt.Fprintf(&b, "| ingestion (append + project) | %s | %s | %.0f |\n",
			count(r.Ingest.Events), roundDur(time.Duration(r.Ingest.Seconds*float64(time.Second))), r.Ingest.PerSecond)
	} else {
		b.WriteString("| ingestion (append + project) | — | not measured | — |\n")
	}
	if r.Replay.Measured {
		fmt.Fprintf(&b, "| full replay (project only, %d events/tx) | %s | %s | %.0f |\n",
			r.Replay.BatchSize, count(r.Replay.Events),
			roundDur(time.Duration(r.Replay.Seconds*float64(time.Second))), r.Replay.PerSecond)
	} else {
		b.WriteString("| full replay (project only) | — | not measured | — |\n")
	}
	b.WriteString("\n")

	if len(r.Latencies) == 0 {
		// A replay-only run measured no queries; an empty table would read as "all zero".
		b.WriteString("## Query latency\n\nNot measured in this run.\n\n")
		return b.String() + r.criteriaMarkdown()
	}
	b.WriteString("## Query latency\n\n")
	fmt.Fprintf(&b, "%d randomized queries per family, milliseconds.\n\n", r.Queries)
	b.WriteString("| query | n | p50 | p95 | p99 | max | mean |\n|---|---:|---:|---:|---:|---:|---:|\n")
	for _, l := range r.Latencies {
		fmt.Fprintf(&b, "| %s | %d | %.1f | %.1f | %.1f | %.1f | %.1f |\n",
			l.Name, l.Count, l.P50Ms, l.P95Ms, l.P99Ms, l.MaxMs, l.MeanMs)
	}
	b.WriteString("\n")
	for _, l := range r.Latencies {
		if l.Detail != "" {
			fmt.Fprintf(&b, "- `%s` — %s\n", l.Name, l.Detail)
		}
	}
	b.WriteString("\n")

	return b.String() + r.criteriaMarkdown()
}

// criteriaMarkdown is the verdict table and the reproduction line, which every run ends with —
// including a replay-only one, which stops before the query section.
func (r *BenchReport) criteriaMarkdown() string {
	var b strings.Builder
	b.WriteString("## Criteria\n\n")
	b.WriteString("| source | criterion | target | measured | result |\n|---|---|---|---|---|\n")
	for _, c := range r.Criteria {
		result := "FAIL"
		switch {
		case c.Skipped:
			result = "skipped"
		case c.Passed:
			result = "pass"
		}
		if !c.Gate && c.Passed {
			result = "pass (goal)"
		} else if !c.Gate && !c.Passed {
			result = "missed (goal)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", c.ID, c.What, c.Target, c.Actual, result)
	}
	b.WriteString("\n")
	b.WriteString("Gates are SC-001, SC-002 and the ADR-0002 exit criteria: missing one of the ADR\n")
	b.WriteString("criteria after index tuning is what reopens the storage decision (constitution X),\n")
	b.WriteString("with Apache AGE as the first alternative to evaluate. Rows marked *(goal)* are\n")
	b.WriteString("plan.md performance goals and do not gate the decision.\n\n")
	b.WriteString("Reproduce with:\n\n```\nsre-agent fixture bench --db \"$PG_DSN\" --scale ")
	fmt.Fprintf(&b, "%s --seed %d --queries %d --out docs/benchmarks\n```\n", r.Scale, r.Seed, r.Queries)
	return b.String()
}

// WriteTo writes the Markdown and JSON forms into dir, returning their paths.
func (r *BenchReport) WriteTo(dir string) (markdownPath, jsonPath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("fixture: create %s: %w", dir, err)
	}
	markdownPath = filepath.Join(dir, r.FileStem()+".md")
	jsonPath = filepath.Join(dir, r.FileStem()+".json")
	if err := os.WriteFile(markdownPath, []byte(r.Markdown()), 0o644); err != nil {
		return "", "", fmt.Errorf("fixture: write %s: %w", markdownPath, err)
	}
	encoded, err := r.JSON()
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(jsonPath, encoded, 0o644); err != nil {
		return "", "", fmt.Errorf("fixture: write %s: %w", jsonPath, err)
	}
	return markdownPath, jsonPath, nil
}

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

// describeEnvironment records what the run was measured on. Everything it cannot determine is
// left empty rather than guessed: a wrong CPU model in a committed report is worse than none.
func describeEnvironment(ctx context.Context, store *postgres.Store) (BenchEnvironment, error) {
	env := BenchEnvironment{
		OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUCount: runtime.NumCPU(), GoVersion: runtime.Version(),
		CPU: cpuModel(), MemoryBytes: memoryBytes(),
	}
	var version string
	if err := store.Pool().QueryRow(ctx, `SELECT version()`).Scan(&version); err != nil {
		return env, fmt.Errorf("fixture: read PostgreSQL version: %w", err)
	}
	env.PostgresVersion = strings.TrimSpace(version)

	var host *string
	if err := store.Pool().QueryRow(ctx,
		`SELECT coalesce(host(inet_server_addr()), 'local socket')`).Scan(&host); err == nil && host != nil {
		env.PostgresServer = *host
	}
	return env, nil
}

// cpuModel returns the processor's marketing name, or "" when it cannot be read.
func cpuModel() string {
	switch runtime.GOOS {
	case "darwin":
		return strings.TrimSpace(sysctl("machdep.cpu.brand_string"))
	case "linux":
		return procCPUModel()
	default:
		return ""
	}
}

func procCPUModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), ":")
		if ok && strings.TrimSpace(name) == "model name" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func memoryBytes() int64 {
	switch runtime.GOOS {
	case "darwin":
		v, err := strconv.ParseInt(strings.TrimSpace(sysctl("hw.memsize")), 10, 64)
		if err != nil {
			return 0
		}
		return v
	case "linux":
		return procMemTotal()
	default:
		return 0
	}
}

func procMemTotal() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok || strings.TrimSpace(name) != "MemTotal" {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// sysctl reads one macOS sysctl. A failure is an empty string: the report says "unknown" and
// the run carries on.
func sysctl(key string) string {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", key).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "unknown"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// count renders an integer with thousands separators, because a benchmark report is read by a
// person deciding whether 1000000 is the number they asked for.
func count(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, digit := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(digit)
	}
	return b.String()
}
