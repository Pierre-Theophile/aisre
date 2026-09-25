// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/gcpx"
)

// SC-020's capability half (T174; FR-005): every operation the integration is CAPABLE of issuing is
// on the published read-only list.
//
// The criterion is deliberate about this and the plan repeats it: read-only must be *"verifiable
// from the set of operations the integration can issue, not only from one run's behaviour"*. A run
// that happened to issue nothing but reads proves nothing about the run that reaches a different
// branch, and no amount of recorded corpus closes that gap — so the assertion is over the source of
// the one file that holds a Google client.
//
// Two of the three ways to get this wrong are already closed by the type system and are worth naming
// so this file is not read as doing more than it does:
//
//   - a call site cannot name an unpublished operation, because the constant does not exist and a
//     string literal is refused at runtime by `gcpx.Issuable`;
//   - a call site cannot spend a call without naming an operation, because `Budget.take` is
//     unexported and `Issue` is the only door.
//
// The third is what this file closes: a method that calls its Google client and NEVER GOES THROUGH
// THE BUDGET AT ALL. Nothing in the type system stops that — the client is right there on the
// struct — and it is the realistic mistake, because it is what a method looks like before the
// metering is added. So: every method on a live reader that touches its client must first issue a
// published operation, and the operation must be named by a `gcpx.Op…` constant rather than computed.

// transportSource is the only file in this repository that constructs a per-area Google Cloud client
// (see its own header). If that ever stops being true, the list here grows and this comment is the
// reason to notice.
const transportSource = "transport.go"

func TestEveryLiveReaderMethodIssuesAPublishedOperationBeforeCallingGoogle(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, transportSource, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", transportSource, err)
	}

	type method struct {
		receiver string
		name     string
		ops      []string
		touches  bool // calls something on a field that is not the budget
	}
	var methods []method

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		recv := receiverTypeName(fn.Recv)
		// The live readers are the ones that hold a client. `liveTransport` is the shared holder
		// and has no reader method of its own.
		if !strings.HasPrefix(recv, "live") || recv == "liveTransport" {
			continue
		}
		m := method{receiver: recv, name: fn.Name.Name}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if isBudgetIssue(sel) {
				m.ops = append(m.ops, operationArgument(call))
				return true
			}
			// Anything called on a field of the receiver other than the budget is a reach for
			// the vendor: `s.svc.Instances.List(...)`, `r.services.ListServices(...)`.
			if root := rootFieldOfSelector(sel); root != "" && root != "budget" && root != "lt" {
				m.touches = true
			}
			return true
		})
		methods = append(methods, m)
	}

	if len(methods) == 0 {
		t.Fatalf("no live reader method was found in %s; the parser or the file's shape changed, "+
			"and an assertion over nothing is not an assertion", transportSource)
	}

	published := map[gcpx.Operation]bool{}
	for _, op := range gcpx.Operations() {
		published[op] = true
	}

	issued := map[gcpx.Operation]bool{}
	var checked int
	for _, m := range methods {
		if !m.touches && len(m.ops) == 0 {
			continue // a helper that reaches for nothing; there is nothing to meter
		}
		checked++
		if len(m.ops) == 0 {
			t.Errorf("%s.%s calls its Google client and issues no published operation: the call "+
				"would reach GCP unmetered and would not appear in the request log SC-020 is "+
				"verified from", m.receiver, m.name)
			continue
		}
		for _, name := range m.ops {
			if name == "" {
				t.Errorf("%s.%s names its operation with something other than a gcpx.Op… "+
					"constant; a computed operation name is one the published list cannot be "+
					"checked against", m.receiver, m.name)
				continue
			}
			op, ok := operationOfConstant(name)
			if !ok {
				t.Errorf("%s.%s issues gcpx.%s, which is not a published operation constant",
					m.receiver, m.name, name)
				continue
			}
			if !published[op] {
				t.Errorf("%s.%s issues %q, which is not on the published read-only list",
					m.receiver, m.name, op)
			}
			issued[op] = true
		}
	}

	if checked == 0 {
		t.Fatal("every live reader method was skipped as reaching for nothing; the detector is " +
			"broken and this test would pass over an unmetered transport")
	}

	// The reverse direction is reported, not failed. An operation on the published list that no call
	// site issues is not a defect: the list is what the granted role permits and what this connector
	// may issue, and §2 publishes `run.services.get` and `cloudsql.instances.get` because a role that
	// grants `list` without `get` does not exist. Saying which ones are unexercised is still worth a
	// line in the log, because "published and never issued" is exactly where a reader would assume
	// coverage that is not there.
	var unexercised []string
	for op := range published {
		if !issued[op] {
			unexercised = append(unexercised, string(op))
		}
	}
	sort.Strings(unexercised)
	if len(unexercised) > 0 {
		t.Logf("published but issued by no call site in %s (%d): %s — granted by the role set "+
			"rather than issued, or behind a capability this transport does not construct",
			transportSource, len(unexercised), strings.Join(unexercised, ", "))
	}
}

// TestNoCallSiteSpendsQuotaWithoutNamingAnOperation is the second half, and it is cheap because the
// type system does most of it: `Budget.take` is unexported, so the only way through is `Issue`. What
// remains checkable here is that nobody reintroduced a wrapper.
func TestNoCallSiteSpendsQuotaWithoutNamingAnOperation(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, transportSource, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", transportSource, err)
	}
	var found int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if rootFieldOfSelector(sel) != "budget" && !isBudgetIssue(sel) {
			return true
		}
		found++
		if sel.Sel.Name != "Issue" {
			t.Errorf("%s calls budget.%s; Issue is the only method that names the operation, so "+
				"anything else leaves a hole in the request log", transportSource, sel.Sel.Name)
		}
		return true
	})
	if found == 0 {
		t.Fatalf("no budget call was found in %s; the detector is broken", transportSource)
	}
}

func receiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	switch t := recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if ident, ok := t.X.(*ast.Ident); ok {
			return ident.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// isBudgetIssue recognises `x.lt.budget.Issue(...)` and `x.budget.Issue(...)`.
func isBudgetIssue(sel *ast.SelectorExpr) bool {
	inner, ok := sel.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "budget"
}

// rootFieldOfSelector returns the field a call was made through: for `s.svc.Instances.List` it is
// `svc`, for `r.lt.budget.Issue` it is `budget`. An empty string means the call was not made through
// a field of the receiver at all — a package function, or a local.
func rootFieldOfSelector(sel *ast.SelectorExpr) string {
	// Walk down to the first selector whose X is an identifier.
	cur := sel
	for {
		inner, ok := cur.X.(*ast.SelectorExpr)
		if !ok {
			break
		}
		cur = inner
	}
	if _, ok := cur.X.(*ast.Ident); !ok {
		return ""
	}
	return cur.Sel.Name
}

// operationArgument returns the name of the `gcpx.Op…` constant a call named, or "" if the argument
// was anything else.
func operationArgument(call *ast.CallExpr) string {
	if len(call.Args) < 2 {
		return ""
	}
	sel, ok := call.Args[1].(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "gcpx" || !strings.HasPrefix(sel.Sel.Name, "Op") {
		return ""
	}
	return sel.Sel.Name
}

// operationOfConstant maps a constant's identifier to its value, by looking it up in the published
// list. The mapping is by value rather than by reflection over the package, so a constant renamed
// without its value changing is still found — the value is what GCP sees.
func operationOfConstant(name string) (gcpx.Operation, bool) {
	op, ok := constantValues[name]
	if !ok {
		return "", false
	}
	return op, true
}

// constantValues is the identifier-to-value mapping the AST cannot supply, because the AST of
// transport.go holds only the identifier. It is listed rather than derived, and that is the point:
// adding a call site with a new operation means adding it here, which is a second place a human
// looks at the published list.
var constantValues = map[string]gcpx.Operation{
	"OpRunServicesList":                 gcpx.OpRunServicesList,
	"OpRunServicesGet":                  gcpx.OpRunServicesGet,
	"OpRunRevisionsList":                gcpx.OpRunRevisionsList,
	"OpRunRevisionsGet":                 gcpx.OpRunRevisionsGet,
	"OpLoggingEntriesList":              gcpx.OpLoggingEntriesList,
	"OpMonitoringAlertPoliciesList":     gcpx.OpMonitoringAlertPoliciesList,
	"OpMonitoringTimeSeriesList":        gcpx.OpMonitoringTimeSeriesList,
	"OpMonitoringMetricDescriptorsGet":  gcpx.OpMonitoringMetricDescriptorsGet,
	"OpMonitoringAlertsList":            gcpx.OpMonitoringAlertsList,
	"OpSQLInstancesList":                gcpx.OpSQLInstancesList,
	"OpSQLInstancesGet":                 gcpx.OpSQLInstancesGet,
	"OpSQLOperationsList":               gcpx.OpSQLOperationsList,
	"OpSQLFlagsList":                    gcpx.OpSQLFlagsList,
	"OpContainerClustersList":           gcpx.OpContainerClustersList,
	"OpContainerClustersGet":            gcpx.OpContainerClustersGet,
	"OpComputeForwardingRulesList":      gcpx.OpComputeForwardingRulesList,
	"OpComputeURLMapsList":              gcpx.OpComputeURLMapsList,
	"OpComputeBackendServicesList":      gcpx.OpComputeBackendServicesList,
	"OpComputeNetworkEndpointGroupList": gcpx.OpComputeNetworkEndpointGroupList,
	"OpDNSManagedZonesList":             gcpx.OpDNSManagedZonesList,
	"OpDNSResourceRecordSetList":        gcpx.OpDNSResourceRecordSetList,
	"OpPubSubSubscriptionsPull":         gcpx.OpPubSubSubscriptionsPull,
}
