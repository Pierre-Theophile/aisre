// SPDX-License-Identifier: Apache-2.0

package sanitise

import (
	"fmt"
	"regexp"
	"strings"
)

// The boundary assertion (T042, FR-137, FR-139).
//
// Every call site that turns sanitised values back into bytes calls AssertArtifact on those bytes
// before they go anywhere. It is a second look at the same data from the other end: the field walk
// asks "what is the disposition of this path", and this asks "is there a person in these bytes" —
// which catches the path the field walk never saw, and the path that reaches the writer without
// going through the field walk at all.
//
// It is **not** the independent scan FR-138 requires. That one is `scripts/check-no-secrets.sh`, a
// grep, in another language, with its own test, precisely so that one defect cannot both leak and
// pass. This is the in-process check, and it shares a defect with the rest of this package by
// construction. Both exist; neither is the other's substitute.

// entityKey matches this project's own `<name>@<attribute.key>` entity keys — `payments@otel.service.name`,
// `redis@app.kubernetes.io` — which are email-shaped and are not people. Without this the assertion
// would fire on every graph node in the corpus and would be turned off within a day.
var entityKey = regexp.MustCompile(`@(otel|k8s|app|service|host|container|cloud|db|server|http|url|` +
	`process|net|node|pod|namespace|telemetry|deployment|statefulset|daemonset|faas|messaging|rpc|` +
	`aws|gcp|azure)\.`)

// reservedName matches the documentation names a fixture author may use for a placeholder: RFC 2606
// and RFC 6761. `alice@shop.example` is a placeholder; `alice@shop.io` is a person.
var reservedName = regexp.MustCompile(`@([A-Za-z0-9.-]+\.)?(example|invalid|test|localhost)($|[^A-Za-z0-9.-])` +
	`|@example\.(com|org|net)($|[^A-Za-z0-9.-])`)

// serviceAccount matches a GCP service account, which is email-shaped and is **infrastructure**
// rather than a person.
//
// This exemption was missing and every real GCP recording would have tripped on it: a Cloud Run
// revision carries `serviceAccount: runtime@<project>.iam.gserviceaccount.com`, so the people check
// would fire on the ordinary case and be switched off within a day — the same failure the entity-key
// exemption above exists to prevent. Found by wiring `fixture campaign scan` to a committed twin
// fixture, which is what a gate nothing runs against looks like.
//
// `gserviceaccount.com` is a Google-controlled domain that cannot hold a human mailbox, so matching
// on it is precise rather than generous. It covers the user-managed form
// (`<name>@<project>.iam.gserviceaccount.com`), the default and legacy forms (`@appspot.`,
// `@developer.`, `@cloudservices.`) and every Google-managed service agent (`@gcp-sa-*.iam.`,
// `@serverless-robot-prod.iam.`).
//
// It is an exemption from the PEOPLE check only. A service account is still an infrastructure
// identifier and is still pseudonymised under FR-135; this says it is not a person, not that it is
// recorded verbatim.
var serviceAccount = regexp.MustCompile(`@([A-Za-z0-9.-]+\.)?gserviceaccount\.com($|[^A-Za-z0-9.-])`)

// groupMention matches the three Slack mentions that name nobody.
var groupMention = regexp.MustCompile(`@(here|channel|everyone)($|[^A-Za-z0-9-])`)

// logAttribute matches the log and span attributes a connector writes into a selector, a join key or the
// query a recording records, by name: the environment field (`@env`, 005), the attributes of the published
// version-stamp conventions (pkg/feeder/versionstamp), and the span attributes the apm_topology
// backend and feeder aggregate by (`@duration`, `@error.type`, `@peer.service`). They name a field, never a person, and a recording
// of logs or spans that carry them must not be refused for it. Any other `@name` is still a handle.
var logAttribute = regexp.MustCompile(`@(env|environment|deployment\.environment\.name|version|service\.version|git\.commit\.sha|container\.image\.name|` +
	`container\.image\.digest|faas\.version|duration|error\.type|peer\.service)\.?($|[^A-Za-z0-9._-])`)

// address is the shape an address is written in, for the byte-level pass.
var address = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// PeopleInArtifact returns the first people-shaped value in data that is not an entity key, a
// reserved documentation name, a GCP service account or a group mention — or "" when there is none.
//
// It returns the *class* rather than the value. A report that quoted what it found would put the
// address in a CI log, which is the thing the assertion exists to prevent.
func PeopleInArtifact(data []byte) string {
	text := string(data)
	for _, candidate := range address.FindAllString(text, -1) {
		if entityKey.MatchString(candidate) || reservedName.MatchString(candidate) ||
			serviceAccount.MatchString(candidate) {
			continue
		}
		return "an email address"
	}
	for _, candidate := range regexp.MustCompile(`(?:^|[\s"'])@[A-Za-z][A-Za-z0-9._\-]{2,}`).FindAllString(text, -1) {
		if groupMention.MatchString(candidate) || logAttribute.MatchString(candidate) {
			continue
		}
		return "an @handle"
	}
	return ""
}

// AssertArtifact refuses bytes that must not be written. It takes bytes rather than a path, because
// FR-137 is a statement about the code path: the useful call is the one before the file exists.
func (s *Sanitiser) AssertArtifact(where string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if s.canaries != nil {
		if err := s.canaries.Assert(where, data); err != nil {
			return err
		}
	}
	if class := PeopleInArtifact(data); class != "" {
		return fmt.Errorf("sanitise: %s carries %s; %s. The write was refused before the bytes "+
			"existed (FR-137)", where, class, ErrPeopleNeverHashed)
	}
	if index := strings.Index(string(data), CanaryPrefix); index >= 0 && s.canaries == nil {
		// A canary token in an artifact with no canary set configured is still a canary: the set is
		// how a campaign knows which tokens it planted, not what makes a survivor a failure.
		return fmt.Errorf("sanitise: %s carries a canary token and this run has no canary set "+
			"configured, so it cannot say which campaign planted it. A surviving canary fails "+
			"(FR-139, SC-018)", where)
	}
	return nil
}
