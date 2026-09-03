// Package clock provides an injectable time source.
//
// Mandate TTLs, velocity windows and hold expiry are all time-dependent.
// Injecting the clock makes those testable without sleeping.
package clock

import "time"

// Clock is an injectable time source, letting tests fake "now" instead of
// sleeping for real time to pass.
type Clock interface{ Now() time.Time }
