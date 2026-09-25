// SPDX-License-Identifier: Apache-2.0

package gcpx

import "errors"

// errorsAs is errors.As behind a name, so permissions.go reads as intent rather than plumbing.
func errorsAs(err error, target any) bool { return errors.As(err, target) }
