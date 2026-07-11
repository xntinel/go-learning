# Exercise 24: Select Lease Renewal Backoff Strategy With State Predicates

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A service holding a distributed lock has to renew its lease before it
expires, and how it retries a failed renewal should depend on two things
that don't always agree: how much time is actually left, and how many
renewal attempts have already failed. This module resolves that tension
with a tagless switch whose case order is the entire point — a lease
seconds from expiring must always get fast, predictable retries, even if
it has already failed nine times in a row and would otherwise "deserve"
the slower, jittered backoff reserved for speculative retries. It is
self-contained: its own `go mod init`, code, demo, and test.

## What you'll build

```text
leasebackoff/                independent module: example.com/lease-renewal-backoff-strategy
  go.mod                      go 1.24
  leasebackoff.go               package leasebackoff; SelectStrategy(timeUntilExpiry, retryCount) string
  cmd/demo/main.go              runnable demo over five representative lease states
  leasebackoff_test.go          table over both boundaries plus a dedicated priority-ordering case
```

- Implement: `SelectStrategy(timeUntilExpiry time.Duration, retryCount int) string` — a tagless switch that checks urgency (time until expiry) before it ever looks at retry count.
- Test: a table covering the urgent-window boundary, the jittered-retry-count boundary, and — the case that matters most — a lease with very little time left but a high retry count, proving urgency wins.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/leasebackoff/cmd/demo
cd ~/go-exercises/leasebackoff
go mod init example.com/lease-renewal-backoff-strategy
go mod edit -go=1.24
```

### Why urgency has to be the first case, not the second

`SelectStrategy` has exactly the shape the concepts file warns about most:
two predicates that can both be true for the same input, where getting the
order backwards produces a plausible-looking but wrong answer. A lease with
2 seconds left and 10 failed retries satisfies *both* `timeUntilExpiry <=
urgentWindow` and `retryCount >= jitteredRetryThreshold`. If the switch
checked `retryCount` first, that lease would get `"jittered"` backoff —
random delay, possibly a full multiple of seconds, right as the lease is
about to be lost. Checking urgency first means that case is unreachable for
an urgent renewal: the moment `timeUntilExpiry <= urgentWindow` is true,
`"linear"` wins regardless of what `retryCount` says, because a caller
about to lose its lock needs a fast, bounded retry, not a strategy tuned
for a caller with time to spare. `TestSelectStrategy`'s
`"urgent even with a high retry count"` case exists specifically to pin
this down — it is the one test in the table that would still pass with the
two cases swapped in one of the boundary tests, but fails immediately if
the *ordering* itself regresses.

The two constants, `urgentWindow` and `jitteredRetryThreshold`, are named
instead of inlined so the boundary the tests probe is legible in both
places: `leasebackoff_test.go` references `urgentWindow` and
`jitteredRetryThreshold` directly rather than repeating the literals `5 *
time.Second` and `5`, so a future change to either threshold only has to
happen in one place.

Create `leasebackoff.go`:

```go
// Package leasebackoff selects a retry backoff strategy for renewing a
// distributed lock's lease, using a tagless switch on two predicates: how
// soon the lease expires, and how many renewal attempts have already
// failed. Urgency always takes priority over retry history.
package leasebackoff

import "time"

// urgentWindow is how close to expiry a lease has to be before renewal
// stops caring about retry history and just retries fast and predictably.
const urgentWindow = 5 * time.Second

// jitteredRetryThreshold is how many failed attempts, absent urgency,
// trigger jittered backoff to avoid a synchronized retry storm.
const jitteredRetryThreshold = 5

// SelectStrategy returns "linear", "exponential", or "jittered" for a lock
// renewal attempt. The urgency check is the first case specifically so
// that a lease seconds from expiry always gets fast, predictable retries
// regardless of how many attempts have already failed — a caller that's
// about to lose its lock should never be shuffled into a slower, jittered
// backoff just because it has retried several times already.
func SelectStrategy(timeUntilExpiry time.Duration, retryCount int) string {
	switch {
	case timeUntilExpiry <= urgentWindow:
		return "linear" // urgent: renew fast and predictably no matter the retry history
	case retryCount >= jitteredRetryThreshold:
		return "jittered" // plenty of time left, but many failures: avoid a synchronized retry storm
	default:
		return "exponential" // plenty of time, still early in the retry sequence: back off and grow
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	leasebackoff "example.com/lease-renewal-backoff-strategy"
)

func main() {
	cases := []struct {
		timeUntilExpiry time.Duration
		retryCount      int
	}{
		{-1 * time.Second, 0},
		{2 * time.Second, 10},
		{30 * time.Second, 1},
		{30 * time.Second, 8},
		{2 * time.Minute, 0},
	}

	for _, c := range cases {
		strategy := leasebackoff.SelectStrategy(c.timeUntilExpiry, c.retryCount)
		fmt.Printf("expiry=%-8s retries=%-3d -> %s\n", c.timeUntilExpiry, c.retryCount, strategy)
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
expiry=-1s      retries=0   -> linear
expiry=2s       retries=10  -> linear
expiry=30s      retries=1   -> exponential
expiry=30s      retries=8   -> jittered
expiry=2m0s     retries=0   -> exponential
```

### Tests

`TestSelectStrategy` runs a table over an already-expired lease (with both
zero and high retry counts, to prove urgency alone decides that row),
both sides of the urgent-window boundary (one nanosecond under it and
exactly at it, both `"linear"`; one nanosecond past it, not), both sides of
the jittered-retry-count boundary, and a final case with ample time
remaining but a high retry count contrasted against a nearly-expired lease
with the same retry count — the pair that proves the switch's priority
ordering, not just its individual boundaries, is correct.

Create `leasebackoff_test.go`:

```go
package leasebackoff

import (
	"testing"
	"time"
)

func TestSelectStrategy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		timeUntilExpiry time.Duration
		retryCount      int
		want            string
	}{
		{"already expired, no retries yet", -1 * time.Second, 0, "linear"},
		{"already expired, many retries: urgency still wins", -1 * time.Second, 20, "linear"},
		{"one nanosecond under the urgent window", urgentWindow - time.Nanosecond, 0, "linear"},
		{"exactly at the urgent window boundary", urgentWindow, 0, "linear"},
		{"one nanosecond past the urgent window, few retries", urgentWindow + time.Nanosecond, 0, "exponential"},
		{"one nanosecond past the urgent window, many retries", urgentWindow + time.Nanosecond, jitteredRetryThreshold, "jittered"},
		{"ample time, low retry count", time.Minute, 2, "exponential"},
		{"ample time, one retry below jittered boundary", time.Minute, jitteredRetryThreshold - 1, "exponential"},
		{"ample time, exactly at jittered boundary", time.Minute, jitteredRetryThreshold, "jittered"},
		{"ample time, well past jittered boundary", time.Minute, 50, "jittered"},
		{"urgent even with a high retry count", 2 * time.Second, 10, "linear"},
	}

	for _, tc := range tests {
		if got := SelectStrategy(tc.timeUntilExpiry, tc.retryCount); got != tc.want {
			t.Errorf("%s: SelectStrategy(%s, %d) = %q, want %q", tc.name, tc.timeUntilExpiry, tc.retryCount, got, tc.want)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The selector is correct when a lease near expiry always resolves to
`"linear"` no matter what its retry count is, when both boundaries land on
their inclusive side (`<=` and `>=`, matching the table's exact-boundary
cases), and when the ordering itself — not just each predicate in
isolation — is covered by a test built to fail if the cases were swapped.
Carry this forward: whenever two predicates in a tagless switch can both be
true for the same input, the case order encodes a priority decision, and
that decision deserves its own test — checking each boundary independently
is not the same as checking that the more urgent condition actually wins
when both fire together.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless switch form and case ordering.
- [etcd: Lease](https://etcd.io/docs/latest/learning/api/#lease-api) — a real distributed lease API this exercise models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-sharding-key-router-with-hashing.md](23-sharding-key-router-with-hashing.md) | Next: [25-protocol-version-router-with-cascading-fallback.md](25-protocol-version-router-with-cascading-fallback.md)
