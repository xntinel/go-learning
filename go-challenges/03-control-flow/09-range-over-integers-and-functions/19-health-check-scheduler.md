# Exercise 19: Health Check Scheduler — `for i := range endpoints` Probing Services with Exponential Backoff

**Nivel: Intermedio** — validacion rapida (un test corto).

A service mesh's readiness probe has to check every registered endpoint, but
a single flaky endpoint should not be marked unhealthy on its first failed
attempt -- it needs a few retries with backoff before the scheduler gives up
on it, and a shutdown signal has to stop the whole probe run promptly rather
than finishing every endpoint regardless. Driving the endpoint list with a
plain `for i := range endpoints` counted loop and nesting a bounded retry
loop with exponential backoff inside it produces exactly that behavior as an
`iter.Seq[Status]`. This exercise is an independent module with its own `go
mod init`.

## What you'll build

```text
healthcheck/               independent module: example.com/health-check-scheduler
  go.mod                   module example.com/health-check-scheduler
  healthcheck.go           Status, Probe
  cmd/
    demo/
      main.go              runnable demo: three endpoints, one that never recovers
  healthcheck_test.go      retries-with-backoff, context-cancel, early-stop, panic
```

Implement: `Probe(ctx, endpoints, maxAttempts, base, sleep, check) iter.Seq[Status]` yielding one `Status{Endpoint, Healthy, Attempts}` per endpoint, retrying a failing endpoint with delays that double from `base` up to `maxAttempts` tries; panics if `maxAttempts < 1`.
Test: three endpoints where one succeeds immediately, one succeeds on its third attempt, and one never succeeds all yield the right `Status` and the right doubling backoff schedule; a `check` that cancels the context mid-run stops the probe after the current endpoint; a consumer break stops after one endpoint; `maxAttempts=0` panics.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

The outer `for i := range endpoints` is a plain counted loop over the
endpoint list -- there is nothing lazy to compute about how many endpoints
there are, so unlike the earlier live-pagination exercises in this lesson,
the count is known up front and a counted loop is exactly the right tool.
For each endpoint, an inner `for a := range maxAttempts` drives the retry
schedule: `check` is injected so tests can script arbitrary success/failure
sequences per endpoint, and `sleep` is injected so the backoff delays are
recorded rather than actually waited on, the same pattern the retry exercise
earlier in this lesson uses. The `ctx.Err()` check runs both before an
endpoint starts and before every retry attempt within it, which is what lets
a cancelled context stop the scan mid-endpoint instead of only between
endpoints -- a probe run that is being shut down should not burn through a
dozen retries on one already-doomed endpoint before noticing.

Create `healthcheck.go`:

```go
package healthcheck

import (
	"context"
	"iter"
	"time"
)

// Status is one endpoint's outcome after Probe finishes retrying it.
type Status struct {
	Endpoint string
	Healthy  bool
	Attempts int
}

// Probe iterates over endpoints with a plain `for i := range endpoints`
// counted loop and yields one Status per endpoint. A failing endpoint is
// retried with exponential backoff -- the delay doubling each round from
// base -- up to maxAttempts times or until it succeeds, whichever comes
// first. sleep is injected so tests exercise the backoff schedule without
// any real waiting, and ctx is checked before every attempt and before every
// yield so a cancelled probe (a shutdown, a deadline) stops promptly instead
// of finishing the remaining endpoints.
func Probe(
	ctx context.Context,
	endpoints []string,
	maxAttempts int,
	base time.Duration,
	sleep func(time.Duration),
	check func(endpoint string, attempt int) bool,
) iter.Seq[Status] {
	if maxAttempts < 1 {
		panic("healthcheck: maxAttempts must be >= 1")
	}
	return func(yield func(Status) bool) {
		for i := range endpoints {
			if ctx.Err() != nil {
				return
			}
			ep := endpoints[i]
			healthy := false
			attempt := 0
			for a := range maxAttempts {
				if ctx.Err() != nil {
					return
				}
				attempt = a + 1
				if check(ep, attempt) {
					healthy = true
					break
				}
				if a < maxAttempts-1 {
					sleep(base << a)
				}
			}
			if !yield(Status{Endpoint: ep, Healthy: healthy, Attempts: attempt}) {
				return
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/health-check-scheduler"
)

func main() {
	endpoints := []string{"api", "auth", "billing"}
	successAt := map[string]int{"api": 1, "auth": 3}

	check := func(endpoint string, attempt int) bool {
		return successAt[endpoint] == attempt
	}
	sleep := func(d time.Duration) {
		fmt.Printf("  backing off %v\n", d)
	}

	for s := range healthcheck.Probe(context.Background(), endpoints, 3, 10*time.Millisecond, sleep, check) {
		fmt.Printf("%s: healthy=%v attempts=%d\n", s.Endpoint, s.Healthy, s.Attempts)
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
api: healthy=true attempts=1
  backing off 10ms
  backing off 20ms
auth: healthy=true attempts=3
  backing off 10ms
  backing off 20ms
billing: healthy=false attempts=3
```

`api` succeeds on the first try, so no backoff is ever logged for it.
`auth` fails twice before succeeding on the third attempt, logging the
doubling `10ms, 20ms` backoff between failed attempts. `billing` has no
entry in `successAt`, so it exhausts all 3 attempts and is reported
unhealthy, logging the same `10ms, 20ms` backoff between its own failures.

### Tests

Create `healthcheck_test.go`:

```go
package healthcheck

import (
	"context"
	"testing"
	"time"
)

func TestProbeRetriesWithBackoffUntilHealthy(t *testing.T) {
	t.Parallel()

	endpoints := []string{"a", "b", "c"}
	successAt := map[string]int{"a": 1, "b": 3} // "c" never succeeds

	var sleeps []time.Duration
	check := func(endpoint string, attempt int) bool {
		return successAt[endpoint] == attempt
	}
	sleep := func(d time.Duration) { sleeps = append(sleeps, d) }

	var got []Status
	for s := range Probe(context.Background(), endpoints, 3, 10*time.Millisecond, sleep, check) {
		got = append(got, s)
	}

	want := []Status{
		{Endpoint: "a", Healthy: true, Attempts: 1},
		{Endpoint: "b", Healthy: true, Attempts: 3},
		{Endpoint: "c", Healthy: false, Attempts: 3},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d statuses, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("status[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	wantSleeps := []time.Duration{
		10 * time.Millisecond, // b attempt 1 fails
		20 * time.Millisecond, // b attempt 2 fails
		10 * time.Millisecond, // c attempt 1 fails
		20 * time.Millisecond, // c attempt 2 fails
	}
	if len(sleeps) != len(wantSleeps) {
		t.Fatalf("got %d sleeps %v, want %d %v", len(sleeps), sleeps, len(wantSleeps), wantSleeps)
	}
	for i := range wantSleeps {
		if sleeps[i] != wantSleeps[i] {
			t.Fatalf("sleep[%d] = %v, want %v", i, sleeps[i], wantSleeps[i])
		}
	}
}

func TestProbeStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	endpoints := []string{"a", "b", "c"}

	check := func(endpoint string, attempt int) bool {
		if endpoint == "a" {
			cancel()
			return true
		}
		return true
	}

	var got []Status
	for s := range Probe(ctx, endpoints, 3, time.Millisecond, func(time.Duration) {}, check) {
		got = append(got, s)
	}

	if len(got) != 1 {
		t.Fatalf("got %d statuses, want 1 (stop after cancel)", len(got))
	}
}

func TestProbeStopsEarlyOnConsumerBreak(t *testing.T) {
	t.Parallel()

	endpoints := []string{"a", "b", "c"}
	check := func(string, int) bool { return true }

	count := 0
	for range Probe(context.Background(), endpoints, 3, time.Millisecond, func(time.Duration) {}, check) {
		count++
		break
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestProbePanicsOnInvalidMaxAttempts(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for maxAttempts < 1")
		}
	}()
	Probe(context.Background(), []string{"a"}, 0, time.Millisecond, func(time.Duration) {}, func(string, int) bool { return true })
}
```

## Review

The endpoint that never recovers is the case most implementations get
subtly wrong: it is tempting to `sleep` after the final failed attempt too,
but that wastes a backoff delay on a decision that has already been made --
the loop is about to exit and yield `Healthy: false` regardless. The `a <
maxAttempts-1` guard is what skips that wasted sleep. The other property
worth calling out is that `ctx.Err()` is checked both between endpoints and
between retries within one endpoint; checking only between endpoints would
let a cancelled probe burn through a full retry budget on whichever
endpoint happened to be in flight when cancellation arrived.

## Resources

- [`context` package documentation](https://pkg.go.dev/context)
- [Kubernetes: liveness, readiness, and startup probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)
- [Go spec: `for` range clause (integers)](https://go.dev/ref/spec#For_range)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-circuit-breaker-state-machine.md](18-circuit-breaker-state-machine.md) | Next: [20-event-ordering-detector.md](20-event-ordering-detector.md)
