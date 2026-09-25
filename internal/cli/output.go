// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Rendering (FR-035, contracts/cli.md).
//
// Every command produces the same answer twice: a table for a person and canonical JSON for a
// machine. The JSON half is not a convenience — it is the serialization golden fixtures are
// written in (graph.CanonicalJSON: sorted keys, RFC 3339 UTC timestamps, protobuf JSON
// mapping), so `--output json` and a recorded golden can be diffed byte for byte.
//
// Tables go through text/tabwriter rather than a table library: one dependency fewer
// (constitution X), and columns that align are all a terminal needs.

// Output formats accepted by --output.
const (
	// OutputTable is the human-readable rendering.
	OutputTable = "table"
	// OutputJSON is the canonical serialization used by goldens.
	OutputJSON = "json"
)

// unknownCell is what a table prints for a value the graph does not have. It is never a
// guessed or zero timestamp (FR-011).
const unknownCell = "-"

// printer writes a command's result in the format the user asked for.
type printer struct {
	out    io.Writer
	format string
}

func newPrinter(out io.Writer, format string) *printer {
	if format == "" {
		format = OutputTable
	}
	return &printer{out: out, format: format}
}

// json reports whether the printer is in machine-readable mode, for a command that has to
// build its two renderings differently.
func (p *printer) json() bool { return p.format == OutputJSON }

// writeJSON renders v canonically. A proto.Message goes through the protobuf JSON mapping;
// anything else through encoding/json. Both are then canonicalized identically.
func (p *printer) writeJSON(v any) error {
	encoded, err := graph.CanonicalJSON(v)
	if err != nil {
		return exitWith(ExitTransport, err)
	}
	if _, err := fmt.Fprintf(p.out, "%s\n", encoded); err != nil {
		return exitWith(ExitTransport, err)
	}
	return nil
}

// writeTable renders headers and rows as an aligned table. A nil or empty rows slice prints
// the header alone, which is a real answer ("nothing matched"), not an error.
func (p *printer) writeTable(headers []string, rows [][]string) error {
	w := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	if len(headers) > 0 {
		if _, err := fmt.Fprintln(w, strings.Join(headers, "\t")); err != nil {
			return exitWith(ExitTransport, err)
		}
	}
	for _, row := range rows {
		if _, err := fmt.Fprintln(w, strings.Join(row, "\t")); err != nil {
			return exitWith(ExitTransport, err)
		}
	}
	if err := w.Flush(); err != nil {
		return exitWith(ExitTransport, err)
	}
	return nil
}

// writeLine prints one plain line, for the prose a table mode adds around its table.
func (p *printer) writeLine(format string, args ...any) error {
	if _, err := fmt.Fprintf(p.out, format+"\n", args...); err != nil {
		return exitWith(ExitTransport, err)
	}
	return nil
}

// writeRaw prints text verbatim, for output that is already formatted (a Markdown report).
func (p *printer) writeRaw(text string) error {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if _, err := io.WriteString(p.out, text); err != nil {
		return exitWith(ExitTransport, err)
	}
	return nil
}

// formatTime renders a timestamp for a table cell: RFC 3339 in UTC, or "-" when the graph has
// no value. An absent timestamp is printed as absent, never as the zero instant.
func formatTime(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return unknownCell
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}
