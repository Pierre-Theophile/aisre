// SPDX-License-Identifier: Apache-2.0

package testkit

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The published page and the enforced surface, compared in both directions (004 FR-004, SC-007;
// contracts/read-only-operations.md §1.5).
//
// This is the assertion that stops the drift an operator cannot see. `docs/connectors/<platform>.md`
// is what an operator approves; the `ReadOnlySurface` in the connector's package is what the process
// enforces. If the two can differ, the approval is of a document rather than of a system.
//
// It lives in the testkit rather than in each connector because it is a check every connector must
// pass, and because a per-connector copy is a per-connector opportunity to weaken it.

// PageSectionTitle is the heading whose table is parsed. Both connector pages use it, and the parse
// is narrow on purpose: an operation named in the prose, or in a scope table, is not a published
// operation.
const PageSectionTitle = "The published read-only operation surface"

// PublishedOperation is one row of the page's table.
type PublishedOperation struct {
	Area string
	Why  string
}

// AssertSurfaceMatchesPage fails if the published page and the enforced surface are not the same
// list, operation, area and reason alike.
//
// Area and Why are compared as well as the operation set, because the page's columns are what an
// operator reads to decide whether to grant a scope: an operation published under the wrong area is
// a permission approved in the wrong place, and a reason that differs from the code's is a
// justification for an operation nobody is issuing.
func AssertSurfaceMatchesPage(t *testing.T, s *feeder.ReadOnlySurface, path string) {
	t.Helper()

	page := ReadPublishedOperations(t, path)
	if len(page) == 0 {
		t.Fatalf("no operation was parsed out of %s's %q table; the parser or the page's table "+
			"changed, and a comparison against nothing is not a comparison", path, PageSectionTitle)
	}

	enforced := map[feeder.ReadOperation]feeder.ReadOperationSpec{}
	for _, op := range s.Operations() {
		spec, _ := s.SpecOf(op)
		enforced[op] = spec
	}

	for op, published := range page {
		spec, ok := enforced[op]
		if !ok {
			t.Errorf("%s publishes %q, which %s's surface does not carry: the page promises an "+
				"operation the process would refuse", path, op, s.Platform())
			continue
		}
		if spec.Area != published.Area {
			t.Errorf("%q is published under area %q and enforced under %q", op, published.Area, spec.Area)
		}
		if spec.Why != published.Why {
			t.Errorf("%q is published for %q and enforced for %q; the reason an operator approves "+
				"has to be the reason the code states", op, published.Why, spec.Why)
		}
	}
	for op := range enforced {
		if _, ok := page[op]; !ok {
			t.Errorf("%s's surface carries %q, which %s does not publish: the process may issue an "+
				"operation the operator never approved", s.Platform(), op, path)
		}
	}
}

// ReadPublishedOperations parses the page's operation table.
//
// The table is read rather than a generated block, because the page is prose an operator reads and a
// generated block would be a second artifact nobody checks. One row carries one operation, so the
// area and the reason can both be compared per operation.
func ReadPublishedOperations(t *testing.T, path string) map[feeder.ReadOperation]PublishedOperation {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the published page: %v", err)
	}
	defer func() { _ = f.Close() }()

	out := map[feeder.ReadOperation]PublishedOperation{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var inSection bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") {
			inSection = strings.Contains(line, PageSectionTitle)
			continue
		}
		if strings.HasPrefix(line, "### ") {
			// A subsection of the section is prose, not the table.
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
		names := backticked(cells[1])
		if len(names) != 1 {
			t.Errorf("%s: row %q names %d operations; one row carries one operation so that its "+
				"area and its reason are comparable", path, line, len(names))
			continue
		}
		out[feeder.ReadOperation(names[0])] = PublishedOperation{
			Area: area,
			Why:  strings.TrimSpace(cells[2]),
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read the published page: %v", err)
	}
	return out
}

// backticked returns the back-quoted spans of a table cell, which is how the page spells an
// operation. A cell's unquoted prose is correctly ignored.
func backticked(cell string) []string {
	var out []string
	rest := cell
	for {
		i := strings.Index(rest, "`")
		if i < 0 {
			return out
		}
		rest = rest[i+1:]
		j := strings.Index(rest, "`")
		if j < 0 {
			return out
		}
		if name := strings.TrimSpace(rest[:j]); name != "" {
			out = append(out, name)
		}
		rest = rest[j+1:]
	}
}

// ReadPublishedRows parses any `## <title>` section's markdown table into rows keyed by the first
// back-quoted span of the first column.
//
// It exists because the read-only surface is not the only table a connector publishes and then has to
// keep in step with its code — a status vocabulary is another, and a state the page rules on that the
// code does not is the same failure in a different place. The markdown parsing is the fragile part and
// the part worth sharing; what the columns MEAN is the connector's, so this returns cells and asserts
// nothing about them.
//
// Rows whose first column carries no back-quoted span are skipped, which is how the header row and its
// rule are ignored without special-casing their spelling.
func ReadPublishedRows(t *testing.T, path, sectionTitle string) map[string][]string {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the published page: %v", err)
	}
	defer func() { _ = f.Close() }()

	out := map[string][]string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var inSection bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") {
			inSection = strings.Contains(line, sectionTitle)
			continue
		}
		if strings.HasPrefix(line, "### ") {
			inSection = false
			continue
		}
		if !inSection || !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 2 {
			continue
		}
		keys := backticked(cells[0])
		if len(keys) != 1 {
			continue
		}
		rest := make([]string, 0, len(cells)-1)
		for _, cell := range cells[1:] {
			rest = append(rest, strings.TrimSpace(cell))
		}
		if _, duplicate := out[keys[0]]; duplicate {
			t.Errorf("%s: %q appears twice in the %q table; two rows for one key means one of them is "+
				"not being compared to anything", path, keys[0], sectionTitle)
		}
		out[keys[0]] = rest
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read the published page: %v", err)
	}
	return out
}
