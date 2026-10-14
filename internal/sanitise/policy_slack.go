// SPDX-License-Identifier: Apache-2.0

package sanitise

// The Slack connector's payloads, as its live poller writes them (006 T007, FR-050).
//
// A poll is already reduced before it reaches this table: the poller keeps only channels created inside
// the declaration horizon, and the recording pre-pass (slack.PreparePayload) blanks every name the
// naming rule's pattern does not match. What is left is a workspace, channel ids, the names of
// incident channels, instants and two flags.
//
//   - the workspace and channel ids are pseudonymised, so a recording still joins a declaration to the
//     report that went back to the same channel;
//   - an incident channel's name is kept: it is what the naming rule parses, and a replay that could not
//     see it could not test the rule (SC-001);
//   - the creator is a person. It has no row here because it needs none: IsPeopleField drops `creator`
//     before the table is consulted, and Validate would refuse a row that tried to keep it;
//   - no message text exists in any payload of this connector: the chat worker reads in memory and
//     writes digests, never payloads (FR-031, FR-036).
func addSlackRows(p *Policy) {
	set := func(path string, rule Rule) { p.fields[CanonicalField(path)] = rule }
	keep := func(path, why string) { set(path, Rule{Disposition: Verbatim, Why: why}) }
	pseudo := func(path string, kind Kind, why string) {
		set(path, Rule{Disposition: Pseudonym, Kind: kind, Why: why})
	}
	root := "slack.channels"
	pseudo(root+".team", KindWorkspace, "the workspace; the suffix of the source id")
	keep(root+".gap_before", "a stated gap in the poller's reading")
	c := root + ".channels[]"
	pseudo(c+".id", KindChannel, "the channel's stable id: the declaration key and the report's target")
	keep(c+".name", "an incident channel's name, which the naming rule parses; every other name is blanked by the pre-pass")
	keep(c+".created", "the declared instant (FR-004a)")
	keep(c+".archived", "a flag")
	keep(c+".shared", "a flag: shared with another organisation")
}
