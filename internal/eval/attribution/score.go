// SPDX-License-Identifier: Apache-2.0

package attribution

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The gate (009 SC-002, SC-003, research R13).
const (
	// PrecisionFloor is SC-002: automatic attributions are right at least 95 % of the time.
	PrecisionFloor = 0.95
	// CalibrationTolerance is SC-003: in every populated bin, the observed accuracy is within ten
	// points of the mean stated confidence.
	CalibrationTolerance = 0.10
	// Bins is how many equal-width confidence bins the calibration has.
	Bins = 10
)

// Aggregate is the name the summary gives the gated layers taken together.
const Aggregate = "aggregate"

// LayerDiscovered is how the evaluation reports an alias-layer attribution that followed aliases
// the attributor discovered itself (rule D1) rather than aliases a person declared.
const LayerDiscovered = "discovered"

// GatedLayers are the layers whose decisions are the attributor's own inference, in report order:
// what SC-002 and SC-003 are measured on, and the aggregate is made of.
//
// An attribution that follows an alias a person declared is reported (the alias row) and not
// gated: its confidence is 1 because a person said so, and when it is wrong the catalog is wrong,
// which is what a correction is for (US3). The correction layer is people's, and is the labels.
func GatedLayers() []string {
	return []string{LayerDiscovered, graph.AttributionLayerProbable, graph.AttributionLayerClassifier}
}

// ReportedLayers are every layer the report lists, gated or not.
func ReportedLayers() []string {
	return append([]string{graph.AttributionLayerAlias}, GatedLayers()...)
}

// Layer is one layer's numbers, or the aggregate's.
type Layer struct {
	Layer string `json:"layer"`
	// Signals is every labelled signal: coverage is measured against it.
	Signals int `json:"signals"`
	// Attributed is the signals the automatic decision named an entry for at this layer.
	Attributed int `json:"attributed"`
	// Entries and Correct count the entries named, and those the label names too.
	Entries int `json:"entries"`
	Correct int `json:"correct"`
	// Precision is Correct/Entries, and 1 when nothing was attributed.
	Precision float64 `json:"precision"`
	// Coverage is Attributed/Signals: the share attributed rather than `unknown`.
	Coverage    float64 `json:"coverage"`
	Calibration []Bin   `json:"calibration"`
}

// Bin is one confidence bin: [Lower, Upper), the last one closed.
type Bin struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
	Count int     `json:"count"`
	// Correct is how many of the entries in the bin the label names.
	Correct int `json:"correct"`
	// Confidence is the mean stated score, Accuracy the observed share right, Gap their distance.
	Confidence float64 `json:"confidence"`
	Accuracy   float64 `json:"accuracy"`
	Gap        float64 `json:"gap"`
}

// Summary is the evaluation's numbers.
type Summary struct {
	Cases   int     `json:"cases"`
	Signals int     `json:"signals"`
	Layers  []Layer `json:"layers"`
	// Unknown is the labelled signals the automatic decision left `unknown`, and of those, the ones
	// whose label is `unknown` too.
	Unknown      int `json:"unknown"`
	UnknownRight int `json:"unknown_right"`
	// Corrected is the signals a person decided about after the attributor, and of those, the ones
	// whose automatic decision the label confirms.
	Corrected      int `json:"corrected"`
	CorrectedRight int `json:"corrected_right"`
}

// Layer returns the named layer's numbers.
func (s Summary) Layer(name string) Layer {
	for _, l := range s.Layers {
		if l.Layer == name {
			return l
		}
	}
	return Layer{Layer: name}
}

type observation struct {
	layer   string
	score   float64
	correct bool
}

// Summarise scores outcomes per automatic layer and in aggregate.
func Summarise(outcomes []Outcome) Summary {
	s := Summary{Signals: len(outcomes)}
	cases := map[string]bool{}
	layers := map[string]*Layer{}
	byLayer := map[string][]observation{}
	for _, name := range append(ReportedLayers(), Aggregate) {
		layers[name] = &Layer{Layer: name, Signals: len(outcomes)}
	}
	for _, o := range outcomes {
		cases[o.Case] = true
		if len(o.Automatic) == 0 {
			s.Unknown++
			if len(o.Expect) == 0 {
				s.UnknownRight++
			}
		}
		if o.Corrected {
			s.Corrected++
			if o.Right() {
				s.CorrectedRight++
			}
		}
		seen := map[string]bool{}
		for _, d := range o.Automatic {
			ob := observation{layer: d.Layer, score: d.Score, correct: slices.Contains(o.Expect, d.Slug)}
			for _, name := range []string{d.Layer, Aggregate} {
				l, ok := layers[name]
				if !ok || (name == Aggregate && !slices.Contains(GatedLayers(), d.Layer)) {
					continue
				}
				l.Entries++
				if ob.correct {
					l.Correct++
				}
				if !seen[name] {
					seen[name] = true
					l.Attributed++
				}
				byLayer[name] = append(byLayer[name], ob)
			}
		}
	}
	s.Cases = len(cases)
	for _, name := range append(ReportedLayers(), Aggregate) {
		l := layers[name]
		l.Precision = 1
		if l.Entries > 0 {
			l.Precision = round4(float64(l.Correct) / float64(l.Entries))
		}
		if l.Signals > 0 {
			l.Coverage = round4(float64(l.Attributed) / float64(l.Signals))
		}
		l.Calibration = calibrate(byLayer[name])
		s.Layers = append(s.Layers, *l)
	}
	return s
}

// calibrate bins observations by stated score; every bin is listed, empty ones with a zero count.
func calibrate(obs []observation) []Bin {
	bins := make([]Bin, Bins)
	sums := make([]float64, Bins)
	for i := range bins {
		bins[i].Lower, bins[i].Upper = round4(float64(i)/Bins), round4(float64(i+1)/Bins)
	}
	for _, o := range obs {
		i := min(int(math.Floor(o.score*Bins)), Bins-1)
		i = max(i, 0)
		bins[i].Count++
		sums[i] += o.score
		if o.correct {
			bins[i].Correct++
		}
	}
	for i := range bins {
		if bins[i].Count == 0 {
			continue
		}
		bins[i].Confidence = round4(sums[i] / float64(bins[i].Count))
		bins[i].Accuracy = round4(float64(bins[i].Correct) / float64(bins[i].Count))
		bins[i].Gap = round4(math.Abs(bins[i].Accuracy - bins[i].Confidence))
	}
	return bins
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }

// Gate returns the reasons the summary fails; none means pass. Aggregate precision must meet
// SC-002's floor, and every populated aggregate bin SC-003's tolerance.
func Gate(s Summary) []string {
	var failures []string
	agg := s.Layer(Aggregate)
	if agg.Entries == 0 {
		return []string{"the attributor inferred nothing on the labelled set, so there is nothing to gate"}
	}
	if agg.Precision < PrecisionFloor {
		failures = append(failures, fmt.Sprintf("aggregate precision %.4f is below %.2f (SC-002): %d of %d automatic attributions are wrong",
			agg.Precision, PrecisionFloor, agg.Entries-agg.Correct, agg.Entries))
	}
	for _, b := range agg.Calibration {
		if b.Count > 0 && b.Gap > CalibrationTolerance+1e-9 {
			failures = append(failures, fmt.Sprintf("bin [%.1f, %.1f): stated %.4f, observed %.4f over %d, %.0f points apart (SC-003)",
				b.Lower, b.Upper, b.Confidence, b.Accuracy, b.Count, b.Gap*100))
		}
	}
	return failures
}

// Report writes the per-layer table, the aggregate calibration, every wrong or missed signal and
// the gate's verdict, as Markdown.
func Report(w io.Writer, outcomes []Outcome, s Summary, failures []string) {
	fmt.Fprintf(w, "### Attribution: precision, coverage and calibration\n\n")
	fmt.Fprintf(w, "%d labelled signals in %d cases; %d left `unknown` (%d labelled `unknown`); %d corrected by a person (%d of them the attributor had right).\n\n",
		s.Signals, s.Cases, s.Unknown, s.UnknownRight, s.Corrected, s.CorrectedRight)
	fmt.Fprintf(w, "| layer | gated | attributed | coverage | entries | correct | precision |\n|---|---|---|---|---|---|---|\n")
	for _, l := range s.Layers {
		gated := "yes"
		if l.Layer == graph.AttributionLayerAlias {
			gated = "no: a person's alias"
		}
		fmt.Fprintf(w, "| %s | %s | %d | %.4f | %d | %d | %.4f |\n", l.Layer, gated, l.Attributed, l.Coverage, l.Entries, l.Correct, l.Precision)
	}
	for _, l := range s.Layers {
		if l.Entries == 0 {
			continue
		}
		fmt.Fprintf(w, "\n#### Calibration, %s\n\n| bin | n | stated | observed | gap |\n|---|---|---|---|---|\n", l.Layer)
		for _, b := range l.Calibration {
			if b.Count == 0 {
				continue
			}
			fmt.Fprintf(w, "| [%.1f, %.1f%s | %d | %.4f | %.4f | %.4f |\n", b.Lower, b.Upper, closing(b), b.Count, b.Confidence, b.Accuracy, b.Gap)
		}
	}
	var wrong []string
	for _, o := range outcomes {
		if !o.Right() {
			got := []string{}
			for _, d := range o.Automatic {
				got = append(got, fmt.Sprintf("%s (%s %s %.4f)", d.Slug, d.Layer, d.Rule, d.Score))
			}
			wrong = append(wrong, fmt.Sprintf("- %s `%s`: attributed [%s], labelled [%s]",
				o.Case, o.Signal, strings.Join(got, ", "), strings.Join(o.Expect, ", ")))
		}
	}
	if len(wrong) > 0 {
		fmt.Fprintf(w, "\n#### Signals the attributor did not get right on its own\n\n%s\n", strings.Join(wrong, "\n"))
	}
	if len(failures) == 0 {
		fmt.Fprintf(w, "\n**Gate: pass** (aggregate precision ≥ %.2f, every populated bin within %.0f points).\n", PrecisionFloor, CalibrationTolerance*100)
		return
	}
	fmt.Fprintf(w, "\n**Gate: FAIL**\n\n")
	for _, f := range failures {
		fmt.Fprintf(w, "- %s\n", f)
	}
}

func closing(b Bin) string {
	if b.Upper >= 1 {
		return "]"
	}
	return ")"
}
