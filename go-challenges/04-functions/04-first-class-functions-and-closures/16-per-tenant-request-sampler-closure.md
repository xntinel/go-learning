# Exercise 16: Per-Tenant Request Sampler as Captured Closure

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A multi-tenant observability pipeline cannot trace every request from every
tenant — the volume would drown the backend — so it samples: a premium
tenant paying for full visibility gets 100% of its requests traced, a
high-volume free tenant gets 1%. `NewSampler` closes over a per-tenant rate
table and a per-tenant hit counter, and every tenant's handler goroutine calls
the exact same closure concurrently, so the captured counters need a mutex.

## What you'll build

```text
sampler/                   independent module: example.com/tenant-sampler
  go.mod                   go 1.24
  sampler.go               NewSampler returns func(tenant string) bool
  cmd/
    demo/
      main.go               drives two tenants at different rates
  sampler_test.go           table test: per-tenant rate, isolation, concurrency
```

- Files: `sampler.go`, `cmd/demo/main.go`, `sampler_test.go`.
- Implement: `NewSampler(rates map[string]int) func(tenant string) bool`, closing over a mutex-guarded `map[string]int` of per-tenant hit counters; a tenant's rate `N` means "sample every Nth request."
- Test: a table drives known tenants through several calls and checks the exact sampled/not-sampled sequence for each rate; a second test proves two samplers never share counters; a third fires 2000 concurrent calls at one tenant and asserts the sampled total is exactly `calls/rate` under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Deterministic sampling instead of a random gate

A production sampler often gates on a random draw against a probability, but a
random gate makes the test either flaky or dependent on injecting a fake RNG
for an outcome nobody can eyeball. This module samples deterministically
instead: rate `N` means "every Nth request for this tenant," computed from a
plain integer counter with no randomness at all. `premium` at rate `1` samples
every request (`counter % 1 == 0` is always true); `free` at rate `100`
samples the 100th, 200th, 300th request, giving a stable, table-testable 1%.
The technique generalizes directly to a real percentage-based sampler — swap
the modulo check for a random draw against `1/rate` — but the deterministic
version is what makes the exact sequence in the test provable.

The counter map is the shared mutable state. Every tenant's HTTP handler
calls the same returned closure from its own goroutine, so `NewSampler`
guards the whole check-then-act — read the tenant's rate, increment its
counter, decide whether this is a multiple of the rate — inside one
`sync.Mutex` critical section. Splitting the increment from the modulo check
into two separate locked sections would let two goroutines interleave between
them and both observe a stale counter value, corrupting the ratio; the fix is
to do the full decision under a single lock acquisition, which is exactly
what the closure below does.

`rates` is copied on entry so the caller cannot mutate the sampler's
configuration after construction by holding onto and changing the map it
passed in.

Create `sampler.go`:

```go
package sampler

import "sync"

// NewSampler returns a closure implementing deterministic per-tenant request
// sampling. rates maps a tenant ID to "sample every Nth request": a rate of 1
// samples every request (100%), a rate of 100 samples one in a hundred (1%).
// A tenant absent from rates, or with a non-positive rate, is never sampled.
//
// The returned closure captures the per-tenant hit counters and a mutex.
// Multiple goroutines call it concurrently (one HTTP handler per request), so
// the counter map and the read-modify-write on each counter must be guarded;
// the whole check-then-act (read counter, increment, decide) happens inside
// one critical section.
func NewSampler(rates map[string]int) func(tenant string) bool {
	var mu sync.Mutex
	counters := make(map[string]int)

	// Defensive copy: the caller's map must not be mutable out from under us
	// after NewSampler returns.
	ratesCopy := make(map[string]int, len(rates))
	for tenant, rate := range rates {
		ratesCopy[tenant] = rate
	}

	return func(tenant string) bool {
		mu.Lock()
		defer mu.Unlock()

		rate, ok := ratesCopy[tenant]
		if !ok || rate <= 0 {
			return false
		}

		counters[tenant]++
		return counters[tenant]%rate == 0
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tenant-sampler"
)

func main() {
	sample := sampler.NewSampler(map[string]int{
		"premium": 1, // 100%
		"free":    4, // 25%, small number for a readable demo
	})

	hits := map[string]int{}
	for range 12 {
		if sample("premium") {
			hits["premium"]++
		}
		if sample("free") {
			hits["free"]++
		}
	}

	fmt.Printf("premium sampled=%d\n", hits["premium"])
	fmt.Printf("free sampled=%d\n", hits["free"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
premium sampled=12
free sampled=3
```

### Tests

Create `sampler_test.go`:

```go
package sampler

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestSamplerAppliesPerTenantRate(t *testing.T) {
	sample := NewSampler(map[string]int{
		"premium": 1,
		"canary":  4,
		"unknown": 0,
	})

	tests := []struct {
		name   string
		tenant string
		want   bool
	}{
		{"premium call 1 always sampled", "premium", true},
		{"premium call 2 always sampled", "premium", true},
		{"canary call 1 of 4", "canary", false},
		{"canary call 2 of 4", "canary", false},
		{"canary call 3 of 4", "canary", false},
		{"canary call 4 of 4", "canary", true},
		{"unknown tenant never sampled", "unknown", false},
		{"unconfigured tenant never sampled", "ghost", false},
	}

	for _, tc := range tests {
		if got := sample(tc.tenant); got != tc.want {
			t.Fatalf("%s: sample(%q) = %v, want %v", tc.name, tc.tenant, got, tc.want)
		}
	}
}

func TestTwoSamplersDoNotShareCounters(t *testing.T) {
	rates := map[string]int{"tenant": 2}
	a := NewSampler(rates)
	b := NewSampler(rates)

	if a("tenant") {
		t.Fatal("a: 1st call sampled, want not sampled (rate=2)")
	}
	if !a("tenant") {
		t.Fatal("a: 2nd call not sampled, want sampled (rate=2)")
	}
	if b("tenant") {
		t.Fatal("b: 1st call sampled, want not sampled — b must not share a's captured counter (which is now at 2, a multiple of the rate)")
	}
}

func TestSamplerConcurrentHitsMatchRate(t *testing.T) {
	sample := NewSampler(map[string]int{"bulk": 10})

	const calls = 2000
	const wantSampled = calls / 10

	var wg sync.WaitGroup
	var sampled atomic.Int64
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sample("bulk") {
				sampled.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := sampled.Load(); got != wantSampled {
		t.Fatalf("sampled = %d, want %d", got, wantSampled)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The first table proves the per-tenant modulo sequence exactly, including that
a rate-0 or unconfigured tenant never samples. The isolation test proves the
structural guarantee every factory in this lesson relies on: two calls to
`NewSampler` allocate two separate counter maps, so `a`'s history never
leaks into `b`'s decisions even when given the same rate table. The
concurrency test is the one that fails without the mutex, or fails if the
critical section were split into a separate read-then-write instead of one
locked check-then-act: 2000 goroutines hammering one tenant's counter must
still land on exactly `calls/rate` sampled, every single run, under `-race`.

## Resources

- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the captured counter map across goroutines.
- [pkg.go.dev: sync/atomic](https://pkg.go.dev/sync/atomic) — the counter the test uses to aggregate results from many goroutines.
- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — how the returned closure captures `mu` and `counters`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-admission-gate-closure-pair.md](15-admission-gate-closure-pair.md) | Next: [17-async-job-queue-backpressure-gate.md](17-async-job-queue-backpressure-gate.md)
