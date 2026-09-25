// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The declared role maps, checked where they are declared rather than where they land.
//
// `feeder.WithJoinKeys` drops a role outside the published set **silently**, and deliberately so: a
// feeder naming a role this build does not know is a feeder built against a later schema, and
// dropping the key is better than refusing an otherwise usable pointer. The cost of that choice is
// that a typo in a role name has no symptom at all — the pointer is simply missing a key nobody
// asked for. So the maps are asserted here, in the package that owns them, where the typo is still
// visible.
func TestEveryRoleTheseMapsDeclareIsOneOfThePublishedFive(t *testing.T) {
	maps := map[string]map[string]string{
		"cloudRunJoinKeys": cloudRunJoinKeys,
		"cloudSQLJoinKeys": cloudSQLJoinKeys,
	}
	for name, roles := range maps {
		if len(roles) == 0 {
			t.Fatalf("%s is empty, so every assertion below is vacuous", name)
		}
		for role, field := range roles {
			if !feeder.ValidJoinRole(role) {
				t.Errorf("%s declares the role %q, which is not one of the published five %v. "+
					"WithJoinKeys drops it without a word, so this is a join key that quietly "+
					"never arrives", name, role, feeder.JoinRoles)
			}
			if field == "" {
				t.Errorf("%s maps %q to an empty field; WithJoinKeys drops that too", name, role)
			}
			if !strings.HasPrefix(field, "resource.labels.") && !strings.HasPrefix(field, "metric.labels.") {
				t.Errorf("%s maps %q to %q, which is not a filterable field of a Monitoring "+
					"filter or a Logging query. The spelling is the one that goes into the query, "+
					"not the one a returned series' label map is keyed by", name, role, field)
			}
		}
	}
}
