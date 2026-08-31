// Package clock provides an injectable time source.
//
// Mandate TTLs, velocity windows and hold expiry are all time-dependent.
// Injecting the clock makes those testable without sleeping.
package clock

import "time"

type Clock interface{ Now() time.Time }
