# Exercise 12: Sliding-Window Log Rate Limiter — Count vs Time

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every public API needs a rate limiter, and the sliding-window-log algorithm --
the one behind GitHub's `X-RateLimit-Remaining` headers and Envoy's
`local_ratelimit` filter -- is a ring buffer wearing a different hat. Instead
of samples or log lines, the ring holds the timestamp of every recently
admitted request, and admission is decided by counting how many of those
timestamps are still inside the trailing window. The ring's capacity bounds
memory, exactly as everywhere else in this lesson; the trap unique to this
application is mistaking that capacity bound for the actual admission rule.

A limiter that only asks "is the ring full?" is answering the wrong question.
Fullness tells you how many requests you have ever recorded since the ring
last drained, not how many of them are still within the window that matters
*right now*. A service that bursts to its limit and then goes quiet should
reopen the moment the window elapses -- a client retrying an hour later has
every right to succeed. A limiter that never re-examines its own timestamps
does not reopen. It latches shut the first time it fills and stays shut
forever, which is a production incident indistinguishable from an outage:
every request gets a 429 with no way out, because nothing in the limiter's
logic was ever asked to look at the clock again.

This module builds `Limiter`, a sliding-window-log rate limiter that takes
the current time as an explicit argument to every decision rather than
reading it from the system clock -- the same discipline that makes the whole
package reproducible under `go test` and, not coincidentally, makes it
trivial to run one limiter per tenant against one shared, injectable clock in
production.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
slidinglog/              module example.com/slidinglog
  go.mod                 go 1.24
  limiter.go             Limiter; New, Allow, Remaining; two sentinel errors
  limiter_test.go         admission table, boundary and expiry cases, the
                          latched-limiter contrast, concurrency, ExampleLimiter_Allow
```

- Files: `limiter.go`, `limiter_test.go`.
- Implement: `New(limit int, window time.Duration) (*Limiter, error)` rejecting a non-positive limit with `ErrInvalidLimit` and a non-positive window with `ErrInvalidWindow`; `(*Limiter).Allow(now time.Time) bool` evicting every timestamp at or before `now-window` before deciding, then recording `now` and admitting if capacity remains; `(*Limiter).Remaining(now time.Time) int` reporting spare capacity without recording anything.
- Test: the basic burst-then-reject sequence; readmission once the window has genuinely elapsed; a table of limit/window/arrival combinations including a single-slot limiter and an arrival landing exactly on the window boundary; `Remaining` agreeing with what `Allow` would do; both sentinel errors via `errors.Is`; the latched-limiter contrast; `Limiter` is safe for concurrent use; and `ExampleLimiter_Allow` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A ring bounds memory; it does not, by itself, bound a window

The concepts file for this lesson warns that a fixed-size ring is a
*count*-based window, not a *time*-based one, and that the two diverge the
moment traffic is bursty or idle. A rate limiter is where that warning stops
being an abstract caveat and becomes the entire correctness argument. The
naive version looks completely reasonable:

```go
func (l *naiveLimiter) Allow() bool {
    if l.count >= l.limit {   // "is the ring full" -- not "is anyone still in the window"
        return false
    }
    l.count++
    return true
}
```

It compiles, it passes a smoke test that sends `limit` requests and checks
the next one is rejected, and it ships. What it never does is look at a
clock. `count` only ever goes up; nothing in this function ever brings it
back down. The first burst of traffic that reaches `limit` requests
permanently exhausts the limiter -- there is no elapsed-time check anywhere
in the admission path that could possibly let a later request back in.

The fix restores the missing half of the algorithm: before counting, evict.
`Allow` walks the ring from its oldest entry and drops every timestamp that
is no longer inside the trailing window, exactly the way `Pop` would drop an
entry in any other ring in this lesson -- except here the eviction is driven
by a time comparison against the caller-supplied `now`, not by a `Push`
needing the slot. Only after that walk does `Allow` compare what remains
against `Limit`. A limiter built this way is self-healing: however long a
burst lasted, however long the resulting silence lasts, the very next
request re-evaluates the window from scratch.

Injecting `now` as a parameter rather than calling `time.Now()` inside
`Allow` is not a testing nicety layered on top -- it is why the algorithm can
be pinned at all. A sliding-window bug like the latched limiter above is
invisible in a manual smoke test (nobody waits a day between curl commands)
and immediately visible in a test that can advance the clock by a
`time.Duration` literal.

Create `limiter.go`:

```go
// Package slidinglog implements a sliding-window-log rate limiter over a
// fixed-capacity ring of timestamps.
//
// It exists to get one detail right that a hand-rolled limiter routinely gets
// wrong: the ring's capacity bounds how many timestamps it can *hold*, but
// whether a request is allowed depends on how many of those timestamps are
// still inside the time window, not on whether the ring is full. A limiter
// that only checks fullness latches shut forever once traffic ever bursts to
// its limit, because nothing ever expires. See the package tests for a
// side-by-side demonstration.
package slidinglog

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Sentinel errors returned by New. Callers should test for them with
// errors.Is rather than by comparing error strings.
var (
	// ErrInvalidLimit means the requested burst limit was not positive.
	ErrInvalidLimit = errors.New("slidinglog: limit must be positive")
	// ErrInvalidWindow means the requested window was not positive.
	ErrInvalidWindow = errors.New("slidinglog: window must be positive")
)

// Limiter allows up to Limit requests in any trailing Window of wall-clock
// time, using the sliding-window-log algorithm: every admitted request's
// timestamp is recorded in a fixed-capacity ring, and admission first evicts
// every timestamp older than now-Window before checking how many remain.
//
// A Limiter is safe for concurrent use by multiple goroutines.
type Limiter struct {
	window time.Duration
	times  []time.Time // fixed backing array, length == limit
	head   int         // next write index
	tail   int         // oldest occupied index
	size   int         // occupied entries; disambiguates head==tail
	mu     sync.Mutex
}

// New returns a Limiter admitting up to limit requests per window. It
// returns ErrInvalidLimit or ErrInvalidWindow if either argument is not
// positive.
func New(limit int, window time.Duration) (*Limiter, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidLimit, limit)
	}
	if window <= 0 {
		return nil, fmt.Errorf("%w: got %s", ErrInvalidWindow, window)
	}
	return &Limiter{
		window: window,
		times:  make([]time.Time, limit),
	}, nil
}

// Limit reports the maximum number of requests this Limiter admits per
// window.
func (l *Limiter) Limit() int { return len(l.times) }

// Allow reports whether a request arriving at now is admitted, and records
// it if so. The clock is supplied by the caller rather than read from
// time.Now, which is what makes the limiter's decisions reproducible in
// tests and lets one process serve many independent simulated clocks.
//
// Allow first evicts every recorded timestamp at or before now-Window: those
// requests are no longer inside the trailing window and must not count
// against the caller. Only after that eviction does it compare the number of
// timestamps still on file against Limit.
func (l *Limiter) Allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-l.window)
	for l.size > 0 && !l.times[l.tail].After(cutoff) {
		l.times[l.tail] = time.Time{} // drop the reference; harmless for a
		// value type here, but the discipline generalizes to a ring of
		// pointers or slices.
		l.tail = (l.tail + 1) % len(l.times)
		l.size--
	}

	if l.size == len(l.times) {
		return false
	}
	l.times[l.head] = now
	l.head = (l.head + 1) % len(l.times)
	l.size++
	return true
}

// Remaining reports how many further requests Allow would admit if called
// again with now, without recording anything. It evicts expired timestamps
// under the same lock as Allow so the two never disagree.
func (l *Limiter) Remaining(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-l.window)
	live := l.size
	tail := l.tail
	for live > 0 && !l.times[tail].After(cutoff) {
		tail = (tail + 1) % len(l.times)
		live--
	}
	return len(l.times) - live
}
```

### Using it

Construct one `Limiter` per thing you are rate-limiting -- per API key, per
tenant, per source IP -- with the burst size and window your policy calls
for, and call `Allow(now)` on every incoming request. Because `Limiter` locks
internally, a single value can be shared by every handler goroutine; there is
no external synchronization the caller needs to add. `Remaining` is what
populates a `X-RateLimit-Remaining` response header without mutating state,
which matters when a client checks its quota before deciding whether to
retry at all.

The one contract every caller must honor is supplying a genuinely
monotonic-enough `now`: two calls to `Allow` with `now` values that go
backwards in time will not corrupt the ring (the eviction loop simply finds
nothing new to evict), but they will make the "remaining capacity" answer
temporarily stale until real time catches back up. In production this means
`time.Now()`, called once per request at the call site -- `Limiter` never
calls it itself, by design.

`ExampleLimiter_Allow` in the test file is the runnable demonstration of this
module: `go test` executes it and compares its stdout against the `// Output:`
comment, so the usage shown there cannot drift from the code.

### Tests

`TestAllowAdmitsUpToLimitThenRejects` and `TestAllowReadmitsAfterWindowElapses`
are the two halves of the algorithm read as a story: fill the limiter, watch
it reject, then advance the clock past the window and watch it open back up.
`TestAllowTable` generalizes that story across limit/window combinations,
including a single-slot limiter that must reuse its one slot immediately
after expiry and an arrival landing exactly on the window boundary --
`now-window` equal to a recorded timestamp counts as expired, not as still
inside the window, which the boundary case pins explicitly.
`TestRemainingMatchesAllow` checks the read-only accessor stays honest before
and after requests, and after the window elapses. `TestNewRejectsInvalidConfig`
covers both sentinel errors.

`TestLatchedLimiterNeverReopens` is the heart of the module. `latchedLimiter`
is unexported and unreachable from the package API; it exists so the test
can put the correct `Limiter` and the naive one through the identical burst,
then jump the clock forward twenty-four hours and show the naive one still
refuses every request while `Limiter` admits the next one immediately. If a
future edit to `Allow` ever drops the eviction loop, this test fails here
instead of in an on-call page about a rate limiter that never recovers.
`TestLimiterIsSafeForConcurrentUse` runs fifty goroutines against one shared
`Limiter` well under its limit and confirms every request was admitted with
no lost updates under `-race`.

Create `limiter_test.go`:

```go
package slidinglog

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// latchedLimiter is the naive rate limiter as it is usually written the
// first time: it admits a request while the ring is not yet full, and
// rejects once it is. It never re-examines whether the timestamps already
// on file are still inside the window, so once traffic ever bursts to the
// configured limit the limiter latches shut and never opens again -- not
// after a second of silence, not after a year. It is never exported and
// exists only so the tests can pin what it gets wrong.
type latchedLimiter struct {
	limit int
	count int
}

func (l *latchedLimiter) Allow(time.Time) bool {
	if l.count >= l.limit {
		return false
	}
	l.count++
	return true
}

func TestAllowAdmitsUpToLimitThenRejects(t *testing.T) {
	t.Parallel()

	l, err := New(3, time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 3; i++ {
		if !l.Allow(epoch) {
			t.Fatalf("request %d: want admitted, got rejected", i)
		}
	}
	if l.Allow(epoch) {
		t.Fatal("4th request within the same instant: want rejected, got admitted")
	}
}

func TestAllowReadmitsAfterWindowElapses(t *testing.T) {
	t.Parallel()

	l, err := New(2, 10*time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !l.Allow(epoch) || !l.Allow(epoch.Add(time.Second)) {
		t.Fatal("first two requests: want both admitted")
	}
	if l.Allow(epoch.Add(2 * time.Second)) {
		t.Fatal("3rd request inside the window: want rejected")
	}
	// Only the request at 0s is now outside the trailing 10s window
	// measured from 10.5s (the one at 1s still is not), so exactly one
	// slot frees up.
	later := epoch.Add(10*time.Second + 500*time.Millisecond)
	if !l.Allow(later) {
		t.Fatal("request after the oldest entry expired: want admitted")
	}
	if l.Allow(later) {
		t.Fatal("second request at the same later instant: want rejected, only one slot freed")
	}
}

func TestAllowTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		limit   int
		window  time.Duration
		arrival []time.Duration // offsets from epoch
		want    []bool
	}{
		{
			name:    "single slot reused after expiry",
			limit:   1,
			window:  10 * time.Second,
			arrival: []time.Duration{0, 5 * time.Second, 10*time.Second + 1},
			want:    []bool{true, false, true},
		},
		{
			name:   "burst then drain in order",
			limit:  2,
			window: 10 * time.Second,
			arrival: []time.Duration{
				0, 1 * time.Second, 2 * time.Second,
				10*time.Second + 500*time.Millisecond,
				11*time.Second + 500*time.Millisecond,
			},
			want: []bool{true, true, false, true, true},
		},
		{
			name:    "arrivals exactly at the boundary are evicted",
			limit:   1,
			window:  5 * time.Second,
			arrival: []time.Duration{0, 5 * time.Second},
			want:    []bool{true, true}, // now-window == the recorded time: no longer "after" cutoff
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l, err := New(tc.limit, tc.window)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			for i, off := range tc.arrival {
				got := l.Allow(epoch.Add(off))
				if got != tc.want[i] {
					t.Fatalf("arrival %d at +%s: got %v, want %v", i, off, got, tc.want[i])
				}
			}
		})
	}
}

func TestRemainingMatchesAllow(t *testing.T) {
	t.Parallel()

	l, err := New(3, 10*time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r := l.Remaining(epoch); r != 3 {
		t.Fatalf("Remaining before any request = %d, want 3", r)
	}
	l.Allow(epoch)
	l.Allow(epoch)
	if r := l.Remaining(epoch); r != 1 {
		t.Fatalf("Remaining after 2 requests = %d, want 1", r)
	}
	after := epoch.Add(11 * time.Second)
	if r := l.Remaining(after); r != 3 {
		t.Fatalf("Remaining after window elapsed = %d, want 3", r)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	if _, err := New(0, time.Second); !errors.Is(err, ErrInvalidLimit) {
		t.Errorf("New(limit=0) error = %v, want ErrInvalidLimit", err)
	}
	if _, err := New(-1, time.Second); !errors.Is(err, ErrInvalidLimit) {
		t.Errorf("New(limit=-1) error = %v, want ErrInvalidLimit", err)
	}
	if _, err := New(5, 0); !errors.Is(err, ErrInvalidWindow) {
		t.Errorf("New(window=0) error = %v, want ErrInvalidWindow", err)
	}
	if _, err := New(5, -time.Second); !errors.Is(err, ErrInvalidWindow) {
		t.Errorf("New(window=-1s) error = %v, want ErrInvalidWindow", err)
	}
}

// TestLatchedLimiterNeverReopens is the whole point of the module: a limiter
// that treats "ring is full" as the sole rejection signal, instead of first
// evicting timestamps that are no longer inside the window, latches shut the
// first time traffic bursts to its limit and rejects every request forever
// after -- even a year later. The correct Limiter, given the identical
// sequence of arrivals, opens back up the moment the window has genuinely
// elapsed.
func TestLatchedLimiterNeverReopens(t *testing.T) {
	t.Parallel()

	naive := &latchedLimiter{limit: 2}
	correct, err := New(2, time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 2; i++ {
		naive.Allow(epoch)
		correct.Allow(epoch)
	}

	farFuture := epoch.Add(24 * time.Hour)
	if naive.Allow(farFuture) {
		t.Fatal("latchedLimiter admitted a request a day later; it was expected to stay latched shut")
	}
	if !correct.Allow(farFuture) {
		t.Fatal("Limiter rejected a request a day after the window elapsed; it should have reopened")
	}
}

func TestLimiterIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	l, err := New(1000, time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	var admitted int32
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if l.Allow(epoch.Add(time.Duration(i) * time.Millisecond)) {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if admitted != 50 {
		t.Fatalf("admitted = %d, want 50 (limit 1000 was never approached)", admitted)
	}
}

// ExampleLimiter_Allow is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleLimiter_Allow() {
	l, err := New(2, 10*time.Second)
	if err != nil {
		panic(err)
	}

	now := epoch
	fmt.Println("t=0s ", l.Allow(now))
	fmt.Println("t=1s ", l.Allow(now.Add(1*time.Second)))
	fmt.Println("t=2s ", l.Allow(now.Add(2*time.Second)))

	later := now.Add(11 * time.Second)
	fmt.Println("t=11s", l.Allow(later), "remaining:", l.Remaining(later))

	// Output:
	// t=0s  true
	// t=1s  true
	// t=2s  false
	// t=11s true remaining: 1
}
```

## Review

`Limiter` is correct when the answer to "am I still rate-limited" depends on
elapsed time, not on how many requests have ever been recorded since the ring
last drained. `Allow` gets that right by evicting every timestamp at or
before `now-Window` before it ever compares the remaining count against
`Limit` -- eviction first, decision second, every single call. The bug this
module exists to name is answering the decision from fullness alone: a
limiter with no eviction step latches shut the instant it first fills and
stays shut, because `count` in that version only ever increases. Around that
core, `New` rejects a non-positive limit or window with `ErrInvalidLimit` /
`ErrInvalidWindow`, both checkable with `errors.Is`; `Remaining` answers the
same question `Allow` would without mutating state; and `Limiter` locks
internally, so one value serves every handler goroutine concurrently.
`ExampleLimiter_Allow` is the executable documentation: `go test` verifies
its output. Run `go test -count=1 -race ./...`.

## Resources

- [`time.Time.After`](https://pkg.go.dev/time#Time.After) — the comparison the eviction loop uses to decide whether a recorded request has aged out of the window.
- [Envoy `local_ratelimit` filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/local_rate_limit_filter) — a production sliding-window limiter with the same count-vs-time boundary.
- [GitHub REST API rate limiting](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api) — the `X-RateLimit-Remaining`/`X-RateLimit-Reset` headers this module's `Remaining` method would back.
- [Go Wiki: time package pitfalls](https://go.dev/wiki/CommonMistakes) — injecting the clock instead of calling `time.Now()` inside logic you want to test.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-power-of-two-reorder-buffer.md](11-power-of-two-reorder-buffer.md) | Next: [13-sharded-ring-buffers-hash-distribution.md](13-sharded-ring-buffers-hash-distribution.md)
