// SPDX-License-Identifier: Apache-2.0

package sanitise

// The Notion connector's payloads, as its live poller writes them (007 T005, FR-023, FR-050, research
// R8).
//
// A read is already reduced before it reaches this table. The client never decodes `created_by`,
// `last_edited_by`, a people property, an icon, a cover or a public URL, and the poller copies out only
// the title, the named summary properties as plain text with user mentions removed, the named entity
// property's values, the named date and Notion's two timestamps. The recording pre-pass
// (notion.PreparePayload) then drops the page URL, which carries the page id in the clear. What is
// left:
//
//   - the workspace, database, data-source and page ids are pseudonymised, so a recording still joins a
//     page to its links across reads;
//   - the title and summary text are kept: they are what retrieval ranks, and a replay that could not
//     see them could not test the ranking. This is why a recording of a real workspace stays private
//     and the public repository carries only a synthetic twin (FR-051);
//   - the entity values and the refs they mapped to are kept: they are what the operator's map reads,
//     and the refs are graph references the rest of a fixture already names;
//   - a person never appears, so there is no row for one; IsPeopleField would drop it first anyway.
func addNotionRows(p *Policy) {
	set := func(path string, rule Rule) { p.fields[CanonicalField(path)] = rule }
	keep := func(path, why string) { set(path, Rule{Disposition: Verbatim, Why: why}) }
	drop := func(path, why string) { set(path, Rule{Disposition: Dropped, Why: why}) }
	pseudo := func(path string, kind Kind, why string) {
		set(path, Rule{Disposition: Pseudonym, Kind: kind, Why: why})
	}
	root := "notion.pages"
	pseudo(root+".workspace", KindWorkspace, "the workspace label; the suffix of the source id")
	keep(root+".binding", "the index of the configured database; how a replay finds its binding")
	pseudo(root+".database", KindDatabase, "the database id: the CONCERNS edge's `database` prop")
	pseudo(root+".data_source", KindDataSource, "the data source id the read queried")
	keep(root+".mode", "incremental or reconcile")
	keep(root+".edited_since", "the lower bound of an incremental read")
	keep(root+".complete", "a flag: whether the read was whole")
	keep(root+".gap", "why a read was not whole; the poller's own words and an HTTP status")
	keep(root+".properties_found[]", "a configured property name some listed page carried")
	keep(root+".outside", "a count of listed pages outside the configured databases")
	keep(root+".end_of_cycle", "a flag: the last read of a cycle")
	pg := root + ".pages[]"
	pseudo(pg+".id", KindPage, "the page id: the document reference")
	drop(pg+".url", "the page URL carries the page id in the clear; a replay rebuilds one from the pseudonym")
	keep(pg+".title", "the page title: half of what retrieval ranks (private recordings only, FR-051)")
	keep(pg+".summary[].property", "a configured property name")
	keep(pg+".summary[].text", "a named summary property as plain text, user mentions removed (FR-023)")
	keep(pg+".values[]", "a value of the configured entity property, which the operator's map reads")
	keep(pg+".authored", "the named date")
	keep(pg+".created", "Notion's created_time: when a first registration is valid from")
	keep(pg+".edited", "Notion's last_edited_time: when a change is valid from")
	keep(pg+".registered", "a flag: an earlier read registered the page")
	keep(pg+".previous[]", "an entity reference the page was linked to, as the operator's map named it")
	pseudo(pg+".conflict", KindDatabase, "the earlier database that holds the page")
	keep(pg+".gone", "trashed, moved or not_found")
	keep(pg+".gone_at", "when the page's links end")
}
