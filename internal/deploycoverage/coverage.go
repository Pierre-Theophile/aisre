// SPDX-License-Identifier: Apache-2.0

// Package deploycoverage measures a deploy feeder's coverage against the platform's own answer
// (feature 004 T113, FR-069, SC-001, SC-002).
//
// The measurement is the one feature 003 made for GCP (internal/feeders/gcp/coverage.go), and it is
// deliberately the least flattering available: not "how many rollouts were produced" but "which of
// the deployments GitHub or Vercel itself lists as completed and in scope have no rollout, by id".
//
// # The platform's answer is read from the payloads, not from the feeder
//
// Both sides come from one recording: the platform's answer from `payloads/`, exactly as the
// platform returned it, and the feeder's from `events.jsonl`. The payloads are decoded HERE, by
// code that shares nothing with the mapper — a comparison that asked the mapper which deployments
// were in scope would ask the thing under test to grade itself, and a mapper that dropped a
// deployment for a wrong reason would agree with itself about it every time.
//
// The price is that the two readings of "completed and in scope" are written twice, and can
// disagree. That is the point: a disagreement is either a feeder defect or a place where this
// package's reading of the platform is wrong, and both are worth a line in the report.
//
// # Differences are enumerated, never summarised
//
// FR-069 says so. "99% of deployments" over a thousand is ten missing rollouts, and which ten decides
// whether the number is fine. So a Report carries every difference by id with the reason, including
// the deployments the platform listed and the scope excluded — reported, rather than silently left
// out of the denominator, so a reader can see they were considered.
package deploycoverage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Platform is the deploy platform a report is about. Each is measured separately: a recording can be
// complete for one and blind for the other, and one blended number would hide that.
type Platform string

// The measured platforms.
const (
	GitHub Platform = "github"
	Vercel Platform = "vercel"
)

// The published reasons. A reason is a sentence a reader acts on, so each says what the difference
// means and not only which side it is on.
const (
	// WhyMissing: the platform lists a completed, in-scope deployment and no rollout names it.
	WhyMissing = "the platform lists it as completed and in scope, and no rollout was produced for it: " +
		"a gap in the feeder"
	// WhyExtra: a rollout names a deployment the platform does not list as completed and in scope.
	WhyExtra = "a rollout was produced for it, and the platform does not list it as completed and in " +
		"scope: the feeder emitted a change the platform's own answer does not support"

	// WhyGitHubEnvironment: the deployment's environment is not on the operator's list (FR-032).
	WhyGitHubEnvironment = "its environment is not on the operator's environment list"
	// WhyGitHubNotSucceeded: GitHub reports no `success` status for it, so nothing in production moved.
	WhyGitHubNotSucceeded = "GitHub reports no success status for it: nothing in production moved"
	// WhyGitHubNoStatuses: the recording holds the deployment and none of its statuses, so the
	// platform's answer about it is unknown rather than negative.
	WhyGitHubNoStatuses = "the recording holds no status for it, so whether it completed is not known"

	// WhyVercelNotProduction: a preview or other non-production deployment, excluded by the environment
	// filter (FR-032, SC-002).
	WhyVercelNotProduction = "not a production deployment, excluded by the environment filter"
	// WhyVercelNotPromoted: a production deployment that was built and never promoted to serve traffic.
	// `READY` alone means built and available, not serving (research §3.1).
	WhyVercelNotPromoted = "a production deployment that was never promoted: built, not serving"
	// WhyVercelRollback: a stated rollback. It restores a deployment the list already carries, so it is
	// a change the deployment list has no row for and is not counted against coverage either way.
	WhyVercelRollback = "a rollback the platform stated, restoring a deployment the list already " +
		"carries; not a deployment of its own"
)

// Difference is one deployment present on one side and not the other, or excluded, with the reason.
type Difference struct {
	// ID is the platform's deployment id, as the recording states it (pseudonymised in a campaign).
	ID string `json:"id"`
	// Why is one of the published reasons, with a detail where one helps (the environment, the
	// states seen).
	Why string `json:"why"`
}

// Report is one platform's coverage over one recording, with every difference named.
type Report struct {
	Platform Platform `json:"platform"`
	// Listed is how many deployments the platform lists as completed and in scope. It is the
	// denominator, stated rather than left for a reader to reconstruct.
	Listed int `json:"listed"`
	// Matched is how many of those have a rollout.
	Matched int `json:"matched"`
	// Missing are listed deployments with no rollout, sorted.
	Missing []Difference `json:"missing"`
	// Extra are rollouts for deployments the platform does not list as completed and in scope, sorted.
	Extra []Difference `json:"extra"`
	// Excluded are deployments the platform listed that are not completed or not in scope, each with
	// its reason — reported so a reader can see they were considered and why they are not counted.
	Excluded []Difference `json:"excluded"`
	// Other are changes the feeder produced that correspond to no deployment row, with the reason
	// they are expected. They count neither for nor against coverage.
	Other []Difference `json:"other"`
}

// Complete reports whether every listed deployment has a rollout and no rollout is unsupported.
//
// Unlike GCP's, an extra counts: a deploy platform's deployment list has no retention horizon that
// would make a rollout the platform no longer lists a correct fact about the past, so a rollout for
// a deployment the platform does not list as completed is a change the platform's answer does not
// support.
func (r Report) Complete() bool { return len(r.Missing) == 0 && len(r.Extra) == 0 }

// Ratio is matched over listed, or 1 when nothing is listed. It is for a dashboard that has already
// been given the lists, and it is the last thing on the type for that reason.
func (r Report) Ratio() float64 {
	if r.Listed == 0 {
		return 1
	}
	return float64(r.Matched) / float64(r.Listed)
}

// String renders the report with every difference named, which is the form FR-069 requires.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d of %d listed deployment(s) have a rollout", r.Platform, r.Matched, r.Listed)
	if r.Complete() {
		b.WriteString(" — complete")
	}
	b.WriteString("\n")
	for _, section := range []struct {
		name string
		diff []Difference
	}{
		{"missing", r.Missing},
		{"extra", r.Extra},
		{"excluded", r.Excluded},
		{"other", r.Other},
	} {
		for _, d := range section.diff {
			fmt.Fprintf(&b, "  %-8s %s — %s\n", section.name, d.ID, d.Why)
		}
	}
	return b.String()
}

// Scope is what "in scope" means for the platforms whose scope is configuration rather than a
// platform fact.
type Scope struct {
	// GitHubEnvironments is the operator's environment list, compared case-insensitively as the
	// feeder's allowlist is. Empty means the list the recording's own run declared in its checkpoint
	// (`environments=[…]`), and production alone when it declared none.
	GitHubEnvironments []string
}

func environmentIn(list []string, environment string) bool {
	return slices.ContainsFunc(list, func(e string) bool { return strings.EqualFold(e, environment) })
}

// Measure reads one recording and reports each deploy platform it holds a source for, in platform
// order. A recording with neither is an error: coverage over nothing is not complete, it is absent.
func Measure(dir string, scope Scope) ([]Report, error) {
	kinds, err := sourceKinds(dir)
	if err != nil {
		return nil, err
	}
	payloads, err := readPayloads(dir)
	if err != nil {
		return nil, err
	}
	changes, declared, err := readEvents(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}

	var reports []Report
	if kinds[string(GitHub)] {
		environments := scope.GitHubEnvironments
		if len(environments) == 0 {
			environments = declared
		}
		if len(environments) == 0 {
			environments = []string{"production"}
		}
		r, err := measureGitHub(payloads[GitHub], changes, environments)
		if err != nil {
			return nil, err
		}
		reports = append(reports, r)
	}
	if kinds[string(Vercel)] {
		r, err := measureVercel(payloads[Vercel], changes)
		if err != nil {
			return nil, err
		}
		reports = append(reports, r)
	}
	if len(reports) == 0 {
		return nil, fmt.Errorf("deploycoverage: %s declares no GitHub or Vercel source to measure", dir)
	}
	return reports, nil
}

// sourceKinds reads the manifest's source kinds. The platforms measured are the ones the recording
// says it holds, rather than whichever payload shapes happen to be present: a Kubernetes feeder also
// calls a payload `deployments`, and a recording is not a Vercel recording because of it.
func sourceKinds(dir string) (map[string]bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		return nil, fmt.Errorf("deploycoverage: %w", err)
	}
	var manifest struct {
		Sources []struct {
			Kind string `yaml:"kind"`
		} `yaml:"sources"`
	}
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("deploycoverage: %s: %w", dir, err)
	}
	kinds := map[string]bool{}
	for _, s := range manifest.Sources {
		kinds[s.Kind] = true
	}
	return kinds, nil
}

// --- reading the recording --------------------------------------------------------------------------

// payload is one recorded platform answer, as the index names it.
type payload struct {
	kind string
	raw  []byte
}

// readPayloads reads `payloads/index.jsonl` and every file it names, grouped by the platform the file
// came from. The platform is read from the file's shape rather than from the manifest: a fixture with
// three sources holds payloads from all three under one index.
func readPayloads(dir string) (map[Platform][]payload, error) {
	index := filepath.Join(dir, "payloads", "index.jsonl")
	f, err := os.Open(index)
	if err != nil {
		return nil, fmt.Errorf("deploycoverage: %w", err)
	}
	defer func() { _ = f.Close() }()

	out := map[Platform][]payload{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry struct {
			Kind string `json:"kind"`
			File string `json:"file"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("deploycoverage: %s: %w", index, err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "payloads", entry.File))
		if err != nil {
			return nil, fmt.Errorf("deploycoverage: %w", err)
		}
		if platform, ok := platformOf(entry.Kind, raw); ok {
			out[platform] = append(out[platform], payload{kind: entry.Kind, raw: raw})
		}
	}
	return out, scanner.Err()
}

// platformOf decides which platform a payload came from. Three feeders call a payload
// `deployments`, so the kind alone does not say; the body does. GitHub's is a bare array, Vercel's an
// object whose list sits under a `deployments` key, and the Kubernetes feeder's a watch event, which
// is neither.
func platformOf(kind string, raw []byte) (Platform, bool) {
	switch kind {
	case "deployment-statuses":
		return GitHub, true
	case "deployments":
		trimmed := strings.TrimSpace(string(raw))
		if strings.HasPrefix(trimmed, "[") {
			return GitHub, true
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) == nil {
			if _, ok := object["deployments"]; ok {
				return Vercel, true
			}
		}
	}
	return "", false
}

// change is one produced ROLLOUT, by the ref it was emitted at.
type change struct {
	namespace, value string
}

// declaredEnvironments reads the list a GitHub run states in its checkpoint note.
var declaredEnvironments = regexp.MustCompile(`environments=\[([^\]]*)\]`)

// readEvents reads the ROLLOUT changes a recording's events hold, and the GitHub environment list its
// run declared. Other change kinds — a configuration change, a release — are not deployments and are
// not what FR-069 compares.
func readEvents(path string) ([]change, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("deploycoverage: %w", err)
	}
	defer func() { _ = f.Close() }()

	var out []change
	var environments []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		var event struct {
			SourceID      string `json:"sourceId"`
			ObserveChange *struct {
				Change struct {
					Kind string `json:"kind"`
				} `json:"change"`
				Ref struct {
					Namespace string `json:"namespace"`
					Value     string `json:"value"`
				} `json:"ref"`
			} `json:"observeChange"`
			SourceCheckpoint *struct {
				Note string `json:"note"`
			} `json:"sourceCheckpoint"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, nil, fmt.Errorf("deploycoverage: %s: %w", path, err)
		}
		if c := event.ObserveChange; c != nil && c.Change.Kind == "ROLLOUT" {
			out = append(out, change{namespace: c.Ref.Namespace, value: c.Ref.Value})
		}
		if cp := event.SourceCheckpoint; cp != nil && strings.HasPrefix(event.SourceID, "github:") {
			if m := declaredEnvironments.FindStringSubmatch(cp.Note); m != nil {
				environments = nil
				for _, e := range strings.Split(m[1], ",") {
					if e = strings.TrimSpace(e); e != "" {
						environments = append(environments, e)
					}
				}
			}
		}
	}
	return out, environments, scanner.Err()
}

// --- GitHub ---------------------------------------------------------------------------------------

// measureGitHub compares GitHub's deployments against the `github.change` rollouts.
//
// GitHub designates no terminal state, so "completed" is read the one way the platform's documented
// states allow without interpretation: a deployment with a `success` status. The key is the
// deployment id, which GitHub makes unique across repositories. A monorepo deployment produces one
// change per target, all naming one deployment, so it is one match and never N.
func measureGitHub(payloads []payload, changes []change, environments []string) (Report, error) {
	type deployment struct {
		environment       string
		statusEnvironment string
		states            []string
		statusesRead      bool
	}
	deployments := map[string]*deployment{}
	for _, p := range payloads {
		switch p.kind {
		case "deployments":
			var list []struct {
				ID          json.Number `json:"id"`
				Environment string      `json:"environment"`
			}
			if err := decodeNumbers(p.raw, &list); err != nil {
				return Report{}, fmt.Errorf("deploycoverage: github deployments: %w", err)
			}
			for _, d := range list {
				id := d.ID.String()
				if deployments[id] == nil {
					deployments[id] = &deployment{}
				}
				deployments[id].environment = d.Environment
			}
		case "deployment-statuses":
			var body struct {
				Deployment json.Number `json:"deployment_id"`
				Statuses   []struct {
					State       string `json:"state"`
					Environment string `json:"environment"`
				} `json:"statuses"`
			}
			if err := decodeNumbers(p.raw, &body); err != nil {
				return Report{}, fmt.Errorf("deploycoverage: github deployment statuses: %w", err)
			}
			id := body.Deployment.String()
			if deployments[id] == nil {
				deployments[id] = &deployment{}
			}
			deployments[id].statusesRead = true
			for _, s := range body.Statuses {
				deployments[id].states = append(deployments[id].states, s.State)
				if s.Environment != "" {
					deployments[id].statusEnvironment = s.Environment
				}
			}
		}
	}

	listed := map[string]bool{}
	report := Report{Platform: GitHub}
	for _, id := range sortedKeys(deployments) {
		d := deployments[id]
		// The deployment states its environment; a status restates it. The deployment's own wins, and
		// the status's is read only where the deployment was not recorded or stated none.
		if d.environment == "" {
			d.environment = d.statusEnvironment
		}
		switch {
		case !environmentIn(environments, d.environment):
			report.Excluded = append(report.Excluded, Difference{ID: id,
				Why: fmt.Sprintf("%s (%q)", WhyGitHubEnvironment, d.environment)})
		case !d.statusesRead:
			report.Excluded = append(report.Excluded, Difference{ID: id, Why: WhyGitHubNoStatuses})
		case !slices.Contains(d.states, "success"):
			report.Excluded = append(report.Excluded, Difference{ID: id,
				Why: fmt.Sprintf("%s (states seen: %s)", WhyGitHubNotSucceeded, statesSeen(d.states))})
		default:
			listed[id] = true
		}
	}

	produced := map[string]bool{}
	for _, c := range changes {
		if c.namespace != "github.change" {
			continue
		}
		if id, ok := githubDeploymentOf(c.value); ok {
			produced[id] = true
		}
	}
	return tally(report, listed, produced), nil
}

// githubDeploymentOf reads the deployment id from a GitHub change ref,
// `repositories/<repo>/deployments/<id>/targets/…`. A ref of another shape — a release, a run with
// no deployment — is not a deployment and is not compared.
func githubDeploymentOf(ref string) (string, bool) {
	parts := strings.Split(ref, "/")
	if len(parts) < 4 || parts[0] != "repositories" || parts[2] != "deployments" {
		return "", false
	}
	return parts[3], true
}

func statesSeen(states []string) string {
	if len(states) == 0 {
		return "none"
	}
	seen := append([]string(nil), states...)
	sort.Strings(seen)
	return strings.Join(slices.Compact(seen), ", ")
}

// --- Vercel ---------------------------------------------------------------------------------------

// measureVercel compares Vercel's production deployments against the `vercel.change` rollouts.
//
// "Completed" is `target=production` and `readySubstate=PROMOTED`: serving, which `READY` alone does
// not mean (research §3.1). A deployment is read at every observation and counts as listed if any
// observation showed it promoted, because a promoted deployment that a later one superseded still
// rolled out.
func measureVercel(payloads []payload, changes []change) (Report, error) {
	type deployment struct {
		target   string
		promoted bool
		substate string
	}
	deployments := map[string]*deployment{}
	for _, p := range payloads {
		if p.kind != "deployments" {
			continue
		}
		var body struct {
			Deployments []struct {
				UID           string `json:"uid"`
				Target        string `json:"target"`
				ReadyState    string `json:"readyState"`
				ReadySubstate string `json:"readySubstate"`
			} `json:"deployments"`
		}
		if err := json.Unmarshal(p.raw, &body); err != nil {
			return Report{}, fmt.Errorf("deploycoverage: vercel deployments: %w", err)
		}
		for _, d := range body.Deployments {
			if deployments[d.UID] == nil {
				deployments[d.UID] = &deployment{}
			}
			seen := deployments[d.UID]
			seen.target, seen.substate = d.Target, d.ReadyState+"/"+d.ReadySubstate
			// Compared as the platform spells them, case aside. `READY` is required here and the feeder
			// does not require it: a promoted deployment that is not ready is one this report should
			// show as a difference rather than agree with.
			if strings.EqualFold(strings.TrimSpace(d.Target), "production") &&
				strings.EqualFold(d.ReadyState, "READY") && strings.EqualFold(d.ReadySubstate, "PROMOTED") {
				seen.promoted = true
			}
		}
	}

	listed := map[string]bool{}
	report := Report{Platform: Vercel}
	for _, uid := range sortedKeys(deployments) {
		d := deployments[uid]
		switch {
		case !strings.EqualFold(strings.TrimSpace(d.target), "production"):
			target := d.target
			if target == "" {
				target = "preview" // Vercel states a preview deployment's target as null
			}
			report.Excluded = append(report.Excluded, Difference{ID: uid,
				Why: fmt.Sprintf("%s (target %s)", WhyVercelNotProduction, target)})
		case !d.promoted:
			report.Excluded = append(report.Excluded, Difference{ID: uid,
				Why: fmt.Sprintf("%s (last seen %s)", WhyVercelNotPromoted, d.substate)})
		default:
			listed[uid] = true
		}
	}

	produced := map[string]bool{}
	for _, c := range changes {
		if c.namespace != "vercel.change" {
			continue
		}
		if strings.HasPrefix(c.value, "rollback/") {
			report.Other = append(report.Other, Difference{ID: c.value, Why: WhyVercelRollback})
			continue
		}
		produced[c.value] = true
	}
	sort.Slice(report.Other, func(i, j int) bool { return report.Other[i].ID < report.Other[j].ID })
	return tally(report, listed, produced), nil
}

// --- shared ---------------------------------------------------------------------------------------

// tally fills the counts and both directions of difference, sorted.
func tally(report Report, listed, produced map[string]bool) Report {
	report.Listed = len(listed)
	for _, id := range sortedKeys(listed) {
		if produced[id] {
			report.Matched++
			continue
		}
		report.Missing = append(report.Missing, Difference{ID: id, Why: WhyMissing})
	}
	for _, id := range sortedKeys(produced) {
		if !listed[id] {
			report.Extra = append(report.Extra, Difference{ID: id, Why: WhyExtra})
		}
	}
	// Empty rather than absent, so the JSON form says "none" as `[]` and a reader never has to decide
	// whether `null` meant none or not measured.
	for _, list := range []*[]Difference{&report.Missing, &report.Extra, &report.Excluded, &report.Other} {
		if *list == nil {
			*list = []Difference{}
		}
	}
	return report
}

// sortedKeys sorts numerically where every key is a number, so deployment 10 follows 9.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, errA := strconv.ParseInt(keys[i], 10, 64)
		b, errB := strconv.ParseInt(keys[j], 10, 64)
		if errA == nil && errB == nil {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
}

// decodeNumbers decodes with numbers kept as written, so a 64-bit deployment id is compared as the
// string the ref carries rather than through a float that would round it.
func decodeNumbers(raw []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	return dec.Decode(v)
}
