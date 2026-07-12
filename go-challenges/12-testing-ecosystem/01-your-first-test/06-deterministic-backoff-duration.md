# Exercise 6: Backoff: Testing time.Duration Without a Clock

The core of a retry policy is a pure function: given the attempt number, how long
should the client wait before trying again? Kept free of jitter and the wall
clock, it is a deterministic `time.Duration` calculator — and that purity is what
makes it a trustworthy first test.

## What you'll build

```text
backoff/                   independent module: example.com/backoff
  go.mod
  backoff.go               Base, MaxBackoff; func Backoff(attempt int) time.Duration
  backoff_test.go          TestBackoff (exact durations + cap), ExampleBackoff
  cmd/
    demo/
      main.go              prints the backoff for attempts 0..10
```

- Files: `backoff.go`, `backoff_test.go`, `cmd/demo/main.go`.
- Implement: `Backoff(attempt int) time.Duration` — `Base * 2^attempt`, capped at `MaxBackoff`, with `attempt < 0` treated as 0.
- Test: exact `time.Duration` values for attempts 0, 1, 2 and the cap at a high attempt.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...`.

### Capped exponential backoff, and why jitter is excluded

Exponential backoff doubles the wait after each failed attempt so a struggling
server is not hammered: `Base`, `2*Base`, `4*Base`, `8*Base`, and so on. Left
unbounded that grows to absurd waits, so it is capped at `MaxBackoff`. A negative
attempt is nonsense from a caller and is normalized to attempt 0 rather than
producing a sub-`Base` or negative duration.

The doubling is computed by multiplying `Base` in a loop and returning `MaxBackoff`
the moment it would exceed the cap. That early return is not just tidy — it avoids
the integer overflow a naive `Base << attempt` would hit for a large attempt,
where the shift wraps a `time.Duration` (an `int64` of nanoseconds) into a
negative value. Bounding the growth as you compute it keeps the result correct for
any `attempt`.

Crucially, this function has *no jitter and no clock*. Real backoff adds a random
jitter so that a thundering herd of clients does not retry in lockstep — but
`rand` would make the function non-deterministic and therefore impossible to pin
with an exact-value test. And it reads no `time.Now`; it only computes a duration.
That is a deliberate separation: the pure, exact, testable core lives here, and
the jitter and the actual sleeping are layered on top where a test injects the
random source and the clock. Injecting those is the subject of lesson 16; keeping
them out here is what lets `TestBackoff` assert `Backoff(2) == 400ms` exactly.

Comparing typed `time.Duration` values uses `!=` (a `Duration` is a comparable
`int64`), and the failure message prints with `%v`, which invokes `Duration`'s
`String` method to render `400ms` rather than `400000000`.

Create `backoff.go`:

```go
package backoff

import "time"

const (
	// Base is the wait after the first failure (attempt 0).
	Base = 100 * time.Millisecond
	// MaxBackoff caps the exponential growth.
	MaxBackoff = 30 * time.Second
)

// Backoff returns the wait before the given retry attempt: Base doubled once per
// attempt, capped at MaxBackoff. A negative attempt is treated as 0. It is pure:
// no clock, no jitter, so it can be pinned by exact-value tests.
func Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := Base
	for range attempt {
		d *= 2
		if d >= MaxBackoff {
			return MaxBackoff
		}
	}
	return d
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/backoff"
)

func main() {
	for attempt := range 11 {
		fmt.Printf("attempt %2d -> %v\n", attempt, backoff.Backoff(attempt))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt  0 -> 100ms
attempt  1 -> 200ms
attempt  2 -> 400ms
attempt  3 -> 800ms
attempt  4 -> 1.6s
attempt  5 -> 3.2s
attempt  6 -> 6.4s
attempt  7 -> 12.8s
attempt  8 -> 25.6s
attempt  9 -> 30s
attempt 10 -> 30s
```

### The tests

Create `backoff_test.go`:

```go
package backoff

import (
	"fmt"
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	t.Parallel()

	// Exact durations: the doubling from Base. Compared with != and printed %v.
	if got, want := Backoff(0), 100*time.Millisecond; got != want {
		t.Errorf("Backoff(0) = %v, want %v", got, want)
	}
	if got, want := Backoff(1), 200*time.Millisecond; got != want {
		t.Errorf("Backoff(1) = %v, want %v", got, want)
	}
	if got, want := Backoff(2), 400*time.Millisecond; got != want {
		t.Errorf("Backoff(2) = %v, want %v", got, want)
	}

	// The cap holds at a high attempt.
	if got, want := Backoff(20), MaxBackoff; got != want {
		t.Errorf("Backoff(20) = %v, want %v (MaxBackoff)", got, want)
	}

	// A negative attempt is treated as attempt 0.
	if got, want := Backoff(-1), Base; got != want {
		t.Errorf("Backoff(-1) = %v, want %v (Base)", got, want)
	}
}

func ExampleBackoff() {
	fmt.Println(Backoff(2))
	// Output: 400ms
}
```

## Review

The calculator is correct when `Backoff(0) == Base`, each attempt doubles the
previous, the result never exceeds `MaxBackoff`, and a negative attempt collapses
to attempt 0. The exact-value assertions are only possible because the function is
pure: no jitter, no clock. If you were tempted to add `rand` for jitter here, the
test could no longer assert an exact duration — which is precisely why jitter and
sleeping are deferred to the injection lesson. The loop-with-early-return caps the
growth before an `int64` shift could overflow into a negative duration. Gate with
`gofmt -l .`, `go vet ./...`, and `go test -count=1 -race ./...`.

## Resources

- [time.Duration](https://pkg.go.dev/time#Duration) — the `int64` nanosecond type and its `String` method.
- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why cap and jitter matter in production.
- [testing package](https://pkg.go.dev/testing) — exact-value assertions with `Errorf`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-pagination-offset-guard.md](05-pagination-offset-guard.md) | Next: [07-config-apply-defaults.md](07-config-apply-defaults.md)
