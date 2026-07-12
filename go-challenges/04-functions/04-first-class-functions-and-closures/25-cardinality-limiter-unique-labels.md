# Exercise 25: Time-Series Cardinality Limiter Rejecting New Label Sets

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A metrics or time-series system's cost and memory footprint scale with
*cardinality* — the number of distinct label-set combinations it has ever
stored — not with the number of samples. An unbounded label (a raw user ID,
a request path with an embedded UUID) can silently create millions of
distinct series and take a system down. `NewLimiter` closes over the set of
label combinations seen so far and a maximum; once the cap is hit, brand-new
combinations are rejected while already-known ones keep flowing, all
guarded by a mutex since ingestion happens from many goroutines writing
samples concurrently.

## What you'll build

```text
cardlimit/                 independent module: example.com/cardinality-limiter
  go.mod                   go 1.24
  cardlimit.go             NewLimiter returns func(labelSet string) bool
  cmd/
    demo/
      main.go               five label sets, one rejected past the cap
  cardlimit_test.go         table test: cap enforced, isolation, concurrency
```

- Files: `cardlimit.go`, `cmd/demo/main.go`, `cardlimit_test.go`.
- Implement: `NewLimiter(max int) func(labelSet string) bool`, closing over a mutex-guarded `map[string]struct{}` of seen label sets.
- Test: a table admits distinct sets up to the cap, confirms repeats of already-seen sets are always allowed (even after the cap is hit), and a brand-new set past the cap is rejected; a second test proves two limiters never share state; a concurrency test fires 500 distinct label sets from goroutines at a cap of 50 and asserts exactly 50 are admitted under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/25-cardinality-limiter-unique-labels/cmd/demo
cd go-solutions/04-functions/04-first-class-functions-and-closures/25-cardinality-limiter-unique-labels
go mod edit -go=1.24
```

### Already-seen sets never consume the cap

`NewLimiter` captures `seen`, a set of label-set strings, and a mutex. The
returned closure's decision has three branches in order: if `labelSet` is
already in `seen`, it is always allowed — a limiter that started rejecting
samples for label combinations it had already accepted would silently drop
legitimate ongoing traffic, which is worse than the cardinality explosion it
was meant to prevent. Only when `labelSet` is genuinely new does the limiter
check whether there is room (`len(seen) < max`); if there is, it inserts the
new set and allows it, and if there is not, it rejects — the new
combination is refused, but every previously-admitted series keeps flowing
untouched.

The check-then-act here is "is this new, and if so is there room" — both
questions answered and acted on inside one lock acquisition. If the length
check and the map insert were separate critical sections, two goroutines
each reporting a different brand-new label set at the moment the cap is
almost full could both observe "room for one more" and both insert, pushing
`len(seen)` one past `max`. The concurrency test drives exactly this
scenario — 500 distinct label sets hitting a cap of 50 from separate
goroutines — and requires the admitted count to land on exactly 50.

Create `cardlimit.go`:

```go
package cardlimit

import "sync"

// NewLimiter returns a closure that caps the number of distinct label-set
// combinations a time-series system will accept, guarded by a mutex. A
// labelSet already seen is always allowed (it does not consume any more of
// the cap); a brand-new labelSet is allowed only while fewer than max
// distinct sets have been seen so far.
//
// A metrics ingestion pipeline calls this from many goroutines writing
// samples concurrently, so the whole check-then-act — is this set known,
// and if not, is there room to admit it — happens inside one critical
// section. Splitting the len check from the map write would let two
// goroutines both see room for "one more" and both insert, overshooting max.
func NewLimiter(max int) func(labelSet string) bool {
	var mu sync.Mutex
	seen := make(map[string]struct{})

	return func(labelSet string) bool {
		mu.Lock()
		defer mu.Unlock()

		if _, ok := seen[labelSet]; ok {
			return true
		}
		if len(seen) >= max {
			return false
		}
		seen[labelSet] = struct{}{}
		return true
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cardinality-limiter"
)

func main() {
	allow := cardlimit.NewLimiter(3)

	labelSets := []string{
		`{route="/a",status="200"}`,
		`{route="/b",status="200"}`,
		`{route="/a",status="200"}`, // repeat: still allowed, doesn't consume cap
		`{route="/c",status="200"}`,
		`{route="/d",status="200"}`, // 4th distinct set: rejected
	}

	for _, ls := range labelSets {
		fmt.Printf("%s: allowed=%v\n", ls, allow(ls))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{route="/a",status="200"}: allowed=true
{route="/b",status="200"}: allowed=true
{route="/a",status="200"}: allowed=true
{route="/c",status="200"}: allowed=true
{route="/d",status="200"}: allowed=false
```

### Tests

Create `cardlimit_test.go`:

```go
package cardlimit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLimiterCapsDistinctLabelSets(t *testing.T) {
	allow := NewLimiter(2)

	tests := []struct {
		name string
		set  string
		want bool
	}{
		{"first distinct set admitted", "a", true},
		{"second distinct set admitted", "b", true},
		{"repeat of first set still allowed", "a", true},
		{"repeat of second set still allowed", "b", true},
		{"third distinct set rejected (cardinality cap)", "c", false},
		{"repeat of an already-seen set still allowed after rejection", "a", true},
	}

	for _, tc := range tests {
		if got := allow(tc.set); got != tc.want {
			t.Fatalf("%s: allow(%q) = %v, want %v", tc.name, tc.set, got, tc.want)
		}
	}
}

func TestTwoLimitersDoNotShareSeenSets(t *testing.T) {
	a := NewLimiter(1)
	b := NewLimiter(1)

	if !a("x") {
		t.Fatal("limiter a: first set refused, want allowed")
	}
	if !b("x") {
		t.Fatal("limiter b: first set refused, want allowed — limiters must not share captured state")
	}
	if b("y") {
		t.Fatal("limiter b: second distinct set allowed, want refused (cap is 1)")
	}
}

func TestLimiterConcurrentAdmitsExactlyMax(t *testing.T) {
	const max = 50
	const distinctSets = 500

	allow := NewLimiter(max)

	var wg sync.WaitGroup
	var admitted atomic.Int64
	for i := range distinctSets {
		labelSet := fmt.Sprintf("set-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if allow(labelSet) {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := admitted.Load(); got != max {
		t.Fatalf("admitted = %d, want %d", got, max)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The first table proves the exact contract: distinct sets are admitted up to
the cap, a repeat of an already-seen set is always allowed — even after the
cap has since been reached by other sets — and only a genuinely new set past
the cap is rejected. The isolation test repeats the structural guarantee
every factory in this lesson relies on. The concurrency test is the one that
would fail if the length check and the map insert were two separate locked
steps instead of one: 500 goroutines each reporting a distinct, never-before-
seen label set against a cap of 50 must still land on exactly 50 admitted,
every run, under `-race`.

## Resources

- [Prometheus docs: Cardinality](https://prometheus.io/docs/practices/instrumentation/#do-not-overuse-labels) — why unbounded label values are a real production incident source this exercise models.
- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the shared seen-set map's full check-then-act.
- [pkg.go.dev: sync/atomic](https://pkg.go.dev/sync/atomic) — the counter the concurrency test uses to aggregate admissions across goroutines.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-feature-flag-evaluator-compiled-rules.md](24-feature-flag-evaluator-compiled-rules.md) | Next: [26-compensating-transaction-unwinding-stack.md](26-compensating-transaction-unwinding-stack.md)
