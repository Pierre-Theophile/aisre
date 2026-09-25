// SPDX-License-Identifier: Apache-2.0

package gcpx_test

import (
	"bufio"
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/gcpx"
)

// SC-020, verified from the integration's own request log (T174; FR-005, FR-112).
//
// The criterion: *"Zero write operations are issued against the organisation's GCP resources: every
// request the integration is capable of issuing is on a published read-only list, verified from its
// own recorded request log... The only state change anywhere is the acknowledgement of messages on
// the operator's dedicated doorbell subscription, and it is declared in the read-only statement."*
//
// Four things have to hold and they are separate claims, so they are separate tests:
//
//  1. every operation on the published list is a read, and the one exception is the doorbell;
//  2. an operation NOT on the list is refused before any quota is spent, on a metered budget and on
//     an unmetered one alike;
//  3. the list in code and the list on the published page are the same list, in both directions;
//  4. the request log is the artifact the first three are read from, it separates the backend half
//     from the feeder's, and its read-only claim FAILS when it should.
//
// The fifth claim — that every operation the connector is CAPABLE of issuing is on the list — is a
// statement about call sites rather than about this package, and is asserted where those call sites
// are: internal/feeders/gcp/readonly_test.go.

func TestEveryPublishedOperationIsAReadAndTheOneExceptionIsTheDoorbell(t *testing.T) {
	t.Parallel()

	ops := gcpx.Operations()
	if len(ops) == 0 {
		t.Fatal("the published operation list is empty; SC-020 would hold over nothing")
	}

	var exceptions []gcpx.Operation
	for _, op := range ops {
		spec, ok := gcpx.SpecOf(op)
		if !ok {
			t.Fatalf("Operations() returned %q but SpecOf does not know it", op)
		}
		if spec.Area == "" {
			t.Errorf("%q names no area, so §1's role table cannot be checked against it", op)
		}
		if spec.Class == "" {
			t.Errorf("%q names no endpoint class, so its calls would be metered against nothing", op)
		}
		if spec.StateChange != "" {
			exceptions = append(exceptions, op)
			continue
		}
		// The published claim is stronger than "not a write": every one of them is a list or a
		// get. Assert the strong form, because the weak one would pass on a `validate` or a
		// `preCheck` that the page says is not there.
		if gcpx.IsWrite(string(op)) {
			t.Errorf("%q is on the published read-only list and classifies as a write", op)
		}
		if verb := verbOfPermission(string(op)); verb != "list" && verb != "get" {
			t.Errorf("%q has verb %q; docs/connectors/gcp.md §2 claims every operation is a list "+
				"or a get, so either the code or the page is wrong", op, verb)
		}
	}

	if len(exceptions) != 1 {
		t.Fatalf("the published list carries %d state-changing operations (%v); SC-020 permits "+
			"exactly one, the doorbell acknowledgement", len(exceptions), exceptions)
	}
	if exceptions[0] != gcpx.OpPubSubSubscriptionsPull {
		t.Errorf("the one declared state change is %q, want the doorbell pull", exceptions[0])
	}
	spec, _ := gcpx.SpecOf(exceptions[0])
	if !strings.Contains(spec.StateChange, "doorbell") {
		t.Errorf("the exception's declared change is %q, which does not name the doorbell; the "+
			"read-only statement and the log must say the same thing", spec.StateChange)
	}
	// And the exception is off by default, which is what makes it a declared exception rather than
	// a dependency (FR-006).
	if !strings.Contains(spec.Area, "off by default") {
		t.Errorf("the doorbell's area is %q and does not say it is off by default", spec.Area)
	}
}

func TestAnUnpublishedOperationIsRefusedBeforeAnyQuotaIsSpent(t *testing.T) {
	t.Parallel()
	// The refusal SC-020 turns on. `compute.instances.delete` is the operation an operator is most
	// afraid of and the one a substring rule would catch; `storage.buckets.list` is the one that
	// matters more, because it is an unimpeachable READ that is nonetheless not on the published
	// page, and a gate that only stopped writes would let it through.
	for _, op := range []gcpx.Operation{
		"compute.instances.delete",
		"cloudsql.instances.export",
		"storage.buckets.list",
		"",
	} {
		usage := gcpx.NewUsage(gcpx.ConsumerFeeder)
		budget := gcpx.NewBudget([]gcpx.ClassLimit{loggingLimit()}, usage)

		err := budget.Issue(context.Background(), op, "proj", "")
		if err == nil {
			t.Errorf("%q was issued; it is not on the published list", op)
			continue
		}
		var unpublished *gcpx.UnpublishedOperationError
		if !errors.As(err, &unpublished) {
			t.Errorf("%q was refused with %v, want a typed UnpublishedOperationError so a caller "+
				"can tell a policy refusal from a quota yield", op, err)
		}
		if _, isYield := gcpx.Yielded(err); isYield {
			t.Errorf("%q was reported as a quota yield; it is a policy refusal and retrying will "+
				"never help", op)
		}
		// No quota was spent: the list is consulted first, so an unpublished operation costs
		// nothing at all.
		for _, row := range usage.Rows() {
			if row.Calls != 0 {
				t.Errorf("%q spent %d calls of %s quota before being refused", op, row.Calls, row.Class)
			}
		}
		// And the attempt is in the log, which is what makes ReadOnlyHonoured able to fail.
		blocked := usage.BlockedRequests()
		if len(blocked) != 1 || blocked[0].Operation != op {
			t.Errorf("the request log holds %+v after an attempt to issue %q; the attempt must be "+
				"recorded or the report would call this run clean", blocked, op)
		}
		if usage.ReadOnlyHonoured() {
			t.Errorf("the run reports read-only honoured after attempting %q", op)
		}
	}
}

func TestAnUnmeteredRunStillRefusesAnUnpublishedOperation(t *testing.T) {
	t.Parallel()
	// A nil budget is a replay from disk or a unit test. "We are not counting calls" is never a
	// reason to stop checking what may be called, and this is the assertion that says so: the
	// refusal is a property of the operation, not of the metering.
	var unmetered *gcpx.Budget
	if err := unmetered.Issue(context.Background(), "compute.instances.delete", "proj", ""); err == nil {
		t.Error("an unmetered run issued an unpublished operation; the read-only list is not a " +
			"budget feature")
	}
	if err := unmetered.Issue(context.Background(), gcpx.OpRunServicesList, "proj", "europe-west1"); err != nil {
		t.Errorf("an unmetered run refused a published read: %v", err)
	}
}

func TestTheRequestLogSeparatesTheBackendHalfAndReportsReadOnly(t *testing.T) {
	t.Parallel()
	// FR-112's half of SC-020: executing a term creates no GCP object. The backend spends the same
	// budget through the same readers, so its rows are in the same log — and FR-113 requires they be
	// legible as the backend's rather than folded into ingestion.
	root := gcpx.NewUsage(gcpx.ConsumerFeeder)
	feeder := gcpx.NewBudget([]gcpx.ClassLimit{loggingLimit()}, root.For(gcpx.ConsumerFeeder))
	backend := gcpx.NewBudget([]gcpx.ClassLimit{loggingLimit()}, root.For(gcpx.ConsumerBackend))
	ctx := context.Background()

	if err := feeder.Issue(ctx, gcpx.OpLoggingEntriesList, "proj", ""); err != nil {
		t.Fatalf("feeder issue: %v", err)
	}
	if err := backend.Issue(ctx, gcpx.OpLoggingEntriesList, "proj", ""); err != nil {
		t.Fatalf("backend issue: %v", err)
	}

	byConsumer := map[gcpx.Consumer][]gcpx.RequestRow{}
	for _, row := range root.AllRequests() {
		byConsumer[row.Consumer] = append(byConsumer[row.Consumer], row)
	}
	for _, consumer := range []gcpx.Consumer{gcpx.ConsumerFeeder, gcpx.ConsumerBackend} {
		rows := byConsumer[consumer]
		if len(rows) != 1 || rows[0].Operation != gcpx.OpLoggingEntriesList {
			t.Errorf("the %s half's request log is %+v, want one logging.logEntries.list row",
				consumer, rows)
			continue
		}
		if rows[0].Calls != 1 {
			t.Errorf("the %s half recorded %d calls, want 1", consumer, rows[0].Calls)
		}
		if !rows[0].ReadOnly() {
			t.Errorf("the %s half's row reports a state change: %q", consumer, rows[0].StateChange)
		}
		if rows[0].First.IsZero() || rows[0].Last.IsZero() {
			t.Errorf("the %s half's row carries no instants, so a log cannot be read as a timeline",
				consumer)
		}
	}
	if !root.ReadOnlyHonoured() {
		t.Error("a run that issued two log reads reports read-only not honoured")
	}
	if changes := root.StateChanges(); len(changes) != 0 {
		t.Errorf("a run that touched no doorbell reports state changes: %+v", changes)
	}
}

func TestTheDoorbellIsTheOnlyStateChangeAndTheLogNamesIt(t *testing.T) {
	t.Parallel()
	// The exception, exercised. It is allowed, it is confined to the one operation, and the log
	// names the change in the words the read-only statement uses — so the report answers SC-020's
	// second half without a reader having to know which row is special.
	usage := gcpx.NewUsage(gcpx.ConsumerFeeder)
	budget := gcpx.NewBudget([]gcpx.ClassLimit{{
		Class: gcpx.ClassPubSubPull, Limit: 60, Period: time.Minute, Scope: gcpx.ScopeProject,
		SteadyStateShare: 0.5, HumanReserve: 0.1,
	}}, usage)

	if err := budget.Issue(context.Background(), gcpx.OpPubSubSubscriptionsPull, "proj", ""); err != nil {
		t.Fatalf("the declared exception was refused: %v", err)
	}
	changes := usage.StateChanges()
	if len(changes) != 1 {
		t.Fatalf("the log reports %d state changes after one doorbell pull, want 1: %+v", len(changes), changes)
	}
	if changes[0].Operation != gcpx.OpPubSubSubscriptionsPull {
		t.Errorf("the state change is %q, want the doorbell pull", changes[0].Operation)
	}
	if !strings.Contains(changes[0].StateChange, "doorbell subscription") {
		t.Errorf("the logged change reads %q and does not name the subscription it is confined to",
			changes[0].StateChange)
	}
	// A declared exception is still honoured read-only: SC-020 permits this one change by name.
	if !usage.ReadOnlyHonoured() {
		t.Error("the declared doorbell exception is reported as a read-only breach")
	}
}

// The published page and the code are the same list (T174, FR-154).
//
// This is the assertion that stops the drift an operator cannot see. docs/connectors/gcp.md §2 is
// what Dana approves; internal/gcpx/requestlog.go is what the process enforces. If they can differ,
// the approval is of a document rather than of a system.
func TestThePublishedPageAndTheEnforcedListAreTheSameList(t *testing.T) {
	t.Parallel()

	page := readPublishedOperations(t, "../../docs/connectors/gcp.md")
	if len(page) == 0 {
		t.Fatal("no operation was parsed out of docs/connectors/gcp.md §2; the parser or the " +
			"page's table changed, and a comparison against nothing is not a comparison")
	}

	enforced := map[gcpx.Operation]bool{}
	for _, op := range gcpx.Operations() {
		enforced[op] = true
	}

	for op := range page {
		if !enforced[op] {
			t.Errorf("docs/connectors/gcp.md §2 publishes %q, which internal/gcpx does not carry: "+
				"the page promises an operation the process would refuse", op)
		}
	}
	for op := range enforced {
		if !page[op] {
			t.Errorf("internal/gcpx carries %q, which docs/connectors/gcp.md §2 does not publish: "+
				"the process may issue an operation the operator never approved", op)
		}
	}

	// The area column has to agree too, because §1 grants a role PER AREA and a mismatched area is
	// a permission granted in the wrong place.
	for op, area := range readPublishedAreas(t, "../../docs/connectors/gcp.md") {
		spec, ok := gcpx.SpecOf(op)
		if !ok {
			continue
		}
		if spec.Area != area {
			t.Errorf("%q is published under area %q and enforced under %q", op, area, spec.Area)
		}
	}
}

// readPublishedOperations pulls the operation names out of §2's table.
//
// It reads the table rather than a generated block on purpose: the page is prose an operator reads,
// and a generated block would be a second artifact nobody checks. The parse is deliberately narrow —
// §2 only, back-ticked names only — so an operation mentioned in §1's role YAML or in the prose is
// not mistaken for a published operation.
func readPublishedOperations(t *testing.T, path string) map[gcpx.Operation]bool {
	t.Helper()
	out := map[gcpx.Operation]bool{}
	for op := range readPublishedAreas(t, path) {
		out[op] = true
	}
	return out
}

func readPublishedAreas(t *testing.T, path string) map[gcpx.Operation]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the published page: %v", err)
	}
	defer func() { _ = f.Close() }()

	out := map[gcpx.Operation]string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var inSection bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") {
			inSection = strings.Contains(line, "The published read-only operation list")
			continue
		}
		if strings.HasPrefix(line, "### ") {
			// A subsection of §2 is prose, not the table.
			inSection = false
			continue
		}
		if !inSection || !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 3 {
			continue
		}
		area := strings.TrimSpace(cells[0])
		if area == "area" || strings.HasPrefix(area, "---") {
			continue // the header and its rule
		}
		for _, name := range backticked(cells[1]) {
			out[gcpx.Operation(name)] = area
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read the published page: %v", err)
	}
	return out
}

// backticked returns the back-quoted spans of a table cell, which is how the page spells an
// operation name. A cell's trailing prose — "— **admin activity only**" — is not back-quoted and is
// correctly ignored.
func backticked(cell string) []string {
	var out []string
	parts := strings.Split(cell, "`")
	for i := 1; i < len(parts); i += 2 {
		name := strings.TrimSpace(parts[i])
		if name != "" && strings.Count(name, ".") >= 2 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// verbOfPermission mirrors internal/gcpx's own verb extraction. It is duplicated here rather than
// exported because a test that calls the code under test to decide what the code under test should
// have done asserts nothing.
func verbOfPermission(permission string) string {
	last := permission
	if i := strings.LastIndex(permission, "."); i >= 0 {
		last = permission[i+1:]
	}
	for i, r := range last {
		if r >= 'A' && r <= 'Z' {
			return last[:i]
		}
	}
	return last
}
