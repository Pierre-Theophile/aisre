// SPDX-License-Identifier: Apache-2.0

// Package chat is the chat worker (006 US3, FR-030–FR-036): what was said in the incident's own
// Slack channel, as counts, instants and links, and never as text.
//
// It answers the two terms of the chat family, `chat_mentions` and `chat_timeline`, which algebra
// 1.1.0 added (ADR-0012). Live, the backend in this package reads the channel's history through
// the reader app, holds the messages in memory for the length of one call, and returns a digest;
// recorded, the same worker answers from a world of those digests. A message's text never leaves
// the call that read it, so a world holds nothing a person wrote.
//
// Three rules the rest of the package enforces:
//
//   - **One channel.** The engine names the investigation's own declared channel and the model
//     cannot name another; the backend refuses a channel outside the workspace it is bound to
//     (`outside_origin`), so a misrouted term is a refusal rather than a read.
//   - **Classes, not words.** What may be asked is a published class from `slack/terms@1`, matched
//     by the patterns below. No model reads a message, so the digest has nothing a model added.
//   - **What people said is a reason to look, never a finding.** "rolled back at 14:40" in a chat
//     is a person's report; the graph's change record is the fact (FR-036).
package chat

import (
	"regexp"
	"slices"

	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// Vocabulary is the published name of the term classes and their patterns. A change to any
// pattern below is a new version, because a digest is only comparable with another digest
// matched under the same patterns.
const Vocabulary = engine.ChatVocabulary

// classPatterns is `slack/terms@1`. Each is case-insensitive and anchored on word boundaries, so
// "deployment" matches deploy and "redeploy" does not count as a rollback.
var classPatterns = map[string]*regexp.Regexp{
	"deploy":    regexp.MustCompile(`(?i)\b(deploy(s|ed|ing|ment|ments)?|re-?deploy(ed|ing)?|releas(e|ed|ing)|ship(ped|ping)?|roll(ed|ing)?[ -]?out|rollout)\b`),
	"flag":      regexp.MustCompile(`(?i)\b(feature[ -]?flags?|flag(s|ged|ging)?|toggl(e|ed|ing)|kill[ -]?switch(es)?)\b`),
	"mitigated": regexp.MustCompile(`(?i)\b(mitigat(e|ed|ing|ion)|resolved|recover(ed|ing|y)|back to normal|stable now|all clear|fixed)\b`),
	"restart":   regexp.MustCompile(`(?i)\b(restart(s|ed|ing)?|reboot(s|ed|ing)?|bounc(e|ed|ing)|recycl(e|ed|ing)|kill(ed)? the pods?)\b`),
	"rollback":  regexp.MustCompile(`(?i)\b(roll(ed|ing)?[ -]?back|rollback|revert(s|ed|ing)?)\b`),
	"scale":     regexp.MustCompile(`(?i)\b(scal(e|ed|ing)[ -]?(up|down|out|in)|autoscal(e|ed|er|ing)|replicas?|add(ed|ing)? (nodes|capacity|instances))\b`),
	"vendor":    regexp.MustCompile(`(?i)\b(vendor|third[ -]party|provider|status ?page|upstream (outage|incident)|their (outage|incident))\b`),
}

func init() {
	// The vocabulary the algebra publishes and the patterns here are one list.
	for _, class := range engine.ChatTermClasses {
		if classPatterns[class] == nil {
			panic("chat: published term class " + class + " has no pattern in " + Vocabulary)
		}
	}
	if len(classPatterns) != len(engine.ChatTermClasses) {
		panic("chat: " + Vocabulary + " has a pattern for a class the algebra does not publish")
	}
}

// Classes returns the published classes text matches, sorted.
func Classes(text string) []string {
	var out []string
	for class, pattern := range classPatterns {
		if pattern.MatchString(text) {
			out = append(out, class)
		}
	}
	slices.Sort(out)
	return out
}

// Matches reports whether text matches one class.
func Matches(class, text string) bool {
	pattern := classPatterns[class]
	return pattern != nil && pattern.MatchString(text)
}
