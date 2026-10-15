// SPDX-License-Identifier: Apache-2.0

package sanitise

import (
	"regexp"
	"strings"
)

// Free text that is kept (feature 008 FR-013).
//
// A knowledge section is the one free-text field the agent keeps verbatim, because what a
// postmortem says is what retrieval ranks. So the people and secret rules that elsewhere drop a
// whole field apply here to the shapes inside the text: an e-mail address, an @handle and every
// secret shape scripts/check-no-secrets.sh refuses are replaced by Redacted, live and in a
// recording alike, before a section is stored, indexed, recorded or logged. User mentions never
// get this far: the Notion client drops them before the text is built.
//
// The patterns mirror the commit scan's hard rules on purpose. The scan is the independent check
// (FR-138) and shares no code with this file; a shape added there and not here fails the scan on
// the next recording, which is the order the two are meant to fail in.

// Redacted replaces a removed shape in kept free text.
const Redacted = "[redacted]"

// SecretShapes are the credential shapes kept free text may not carry.
var SecretShapes = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}|AIza[0-9A-Za-z_-]{35}|gh[pousr]_[A-Za-z0-9]{36}|xox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]{12,}`),
	regexp.MustCompile(`postgres(ql)?://[A-Za-z0-9_.-]+:[A-Za-z0-9_.%-]+@[A-Za-z0-9_.:-]+`),
	regexp.MustCompile(`\b(sk|pk|rk)-[A-Za-z0-9_-]{20,}|\bntn_[A-Za-z0-9]{40,}|\bsecret_[A-Za-z0-9]{43}\b|\bglpat-[A-Za-z0-9_-]{16,}|\bnpm_[A-Za-z0-9]{30,}|\bhf_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|apikey|access[_-]?token|secret[_-]?key|password|passwd)["']?\s*[:=]\s*["']?[^\s"']{8,}`),
}

// handle is an @handle at the start of the text or after a space, as the commit scan reads one.
var handle = regexp.MustCompile(`(^|\s)@[A-Za-z][A-Za-z0-9._\-]{2,}`)

// RedactText replaces every e-mail address, @handle and secret shape in text with Redacted and
// reports how many it replaced.
func RedactText(text string) (string, int) {
	n := 0
	for _, shape := range SecretShapes {
		text = shape.ReplaceAllStringFunc(text, func(string) string { n++; return Redacted })
	}
	text = PeopleValuePatterns[0].ReplaceAllStringFunc(text, func(string) string { n++; return Redacted })
	text = handle.ReplaceAllStringFunc(text, func(m string) string {
		n++
		if m[0] == '@' {
			return Redacted
		}
		return m[:1] + Redacted
	})
	return text, n
}

// OnlyRedactions reports text that carries nothing but redactions and punctuation: a section that
// was only a secret, which is skipped rather than stored as "[redacted]".
func OnlyRedactions(text string) bool {
	rest := strings.ReplaceAll(text, Redacted, "")
	for _, r := range rest {
		if r == '_' || ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || r > 0x7f {
			return false
		}
	}
	return true
}
