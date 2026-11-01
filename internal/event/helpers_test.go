// SPDX-License-Identifier: Apache-2.0

package event_test

import "time"

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}
