# Exercise 10: A Fake-Clock Fixture with Cleanup-Verified Expectations

Retry and backoff logic is time-dependent, and testing it against the wall clock
is slow and flaky. Inject a fake `Clock` instead: the test drives virtual time and
asserts the exact sequence of sleeps. The senior touch is a fixture that registers
a `t.Cleanup` verifying every *expected* sleep was consumed — so a forgotten
expectation fails the test automatically instead of passing silently.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
fakeclock/                   independent module: example.com/fakeclock
  go.mod                     go 1.26
  backoff.go                 Clock interface; Retry with exponential backoff via clk.Sleep
  cmd/
    demo/
      main.go                retries a flaky op against a fake clock
  backoff_test.go            newFixedClock(t) fixture; Cleanup-verified sleep expectations
```

- Files: `backoff.go`, `cmd/demo/main.go`, `backoff_test.go`.
- Implement: a `Clock` interface (`Now`, `Sleep`) and `Retry(clk, attempts, base, op)` that sleeps `base<<i` between attempts.
- Test: `newFixedClock(t, expect...)` returns a spy `Clock`, records each `Sleep`, and registers a `t.Cleanup` that fails if the recorded sleeps do not equal the expectations; drive `Retry` deterministically.
- Verify: `go test -count=1 -race ./...`

### Why inject a Clock, and why verify in Cleanup

`Retry` must not call `time.Sleep` directly, or its test would burn real seconds.
It depends on a `Clock` interface with `Now()` and `Sleep(d)`; production wires a
real clock, tests wire a fake that records each sleep and advances a virtual `Now`
by that duration. No wall-clock time passes, and the test can assert the *exact*
backoff schedule — `[base, 2·base, 4·base, ...]` — which is the behavior that
actually matters and is otherwise invisible.

The fixture adds a discipline most hand-rolled fakes miss. `newFixedClock(t,
expect...)` takes the sleeps you *expect* the code to perform and registers a
`t.Cleanup` that, after the test body finishes, compares the recorded sleeps
against that expectation and calls `t.Errorf` on any mismatch. This flips the
default: instead of the test silently passing when the code sleeps too few times
(or the wrong durations), a missed or extra sleep fails automatically at cleanup.
It is the same idea as a mock's "verify all expected calls happened," implemented
with the tool Go already gives you. The spy is mutex-guarded so it is safe if the
code under test ever calls the clock from multiple goroutines, which `-race` checks.

Create `backoff.go`:

```go
package fakeclock

import "time"

// Clock is the time dependency Retry uses, so tests can inject a fake.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

// Retry runs op up to attempts times, sleeping base<<i (exponential backoff)
// after the i-th failed attempt. It returns nil on the first success, or the
// last error if every attempt fails.
func Retry(clk Clock, attempts int, base time.Duration, op func() error) error {
	var err error
	for i := range attempts {
		if err = op(); err == nil {
			return nil
		}
		if i < attempts-1 {
			clk.Sleep(base << i)
		}
	}
	return err
}
```

### The runnable demo

The demo wires a small real-clock-free fake right in `main` (a distinct type from
the test fake) and retries an operation that fails twice then succeeds, printing
the sleeps it performed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/fakeclock"
)

type recordingClock struct {
	now   time.Time
	slept []time.Duration
}

func (c *recordingClock) Now() time.Time { return c.now }

func (c *recordingClock) Sleep(d time.Duration) {
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
}

func main() {
	clk := &recordingClock{now: time.Unix(0, 0).UTC()}
	calls := 0
	err := fakeclock.Retry(clk, 5, 10*time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return errors.New("temporary failure")
		}
		return nil
	})
	fmt.Printf("err=%v calls=%d sleeps=%v\n", err, calls, clk.slept)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
err=<nil> calls=3 sleeps=[10ms 20ms]
```

### The tests

`fakeClock` records sleeps under a mutex and advances virtual `Now`.
`newFixedClock(t, expect...)` returns one and registers a `t.Cleanup` that asserts
the recorded sleeps equal the expectations — so an unmet expectation fails the test
after the body runs. `TestRetrySucceedsAfterFailures` injects a clock expecting
`[10ms, 20ms]`, drives an op that fails twice then succeeds, and asserts success;
the cleanup then verifies exactly those two sleeps happened.
`TestRetryExhausts` expects three sleeps for four always-failing attempts.
`TestClockConcurrentNow` calls `Now` from goroutines under `-race`.

Create `backoff_test.go`:

```go
package fakeclock

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeClock is a spy Clock: it records every Sleep and advances virtual Now.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	slept    []time.Duration
	expected []time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
}

// newFixedClock returns a spy clock and registers a cleanup that fails the test
// unless the recorded sleeps exactly match expect.
func newFixedClock(t *testing.T, expect ...time.Duration) *fakeClock {
	t.Helper()
	c := &fakeClock{now: time.Unix(0, 0).UTC(), expected: expect}
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !slices.Equal(c.slept, c.expected) {
			t.Errorf("clock expectations unmet: slept %v, expected %v", c.slept, c.expected)
		}
	})
	return c
}

func TestRetrySucceedsAfterFailures(t *testing.T) {
	clk := newFixedClock(t, 10*time.Millisecond, 20*time.Millisecond)
	calls := 0
	err := Retry(clk, 5, 10*time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return errors.New("flaky")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
	// Cleanup verifies sleeps == [10ms, 20ms].
}

func TestRetryExhausts(t *testing.T) {
	clk := newFixedClock(t, 10*time.Millisecond, 20*time.Millisecond, 40*time.Millisecond)
	sentinel := errors.New("always fails")
	err := Retry(clk, 4, 10*time.Millisecond, func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("Retry = %v, want sentinel", err)
	}
	// Cleanup verifies three sleeps between four attempts.
}

func TestRetryFirstTrySucceeds(t *testing.T) {
	clk := newFixedClock(t) // expect zero sleeps
	err := Retry(clk, 3, time.Second, func() error { return nil })
	if err != nil {
		t.Fatalf("Retry = %v, want nil", err)
	}
	// Cleanup verifies no sleeps happened.
}

func TestClockConcurrentNow(t *testing.T) {
	clk := newFixedClock(t) // no sleeps expected
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = clk.Now()
		}()
	}
	wg.Wait()
}

func ExampleRetry() {
	clk := &fakeClock{now: time.Unix(0, 0).UTC()}
	err := Retry(clk, 3, time.Second, func() error { return nil })
	fmt.Println(err, clk.slept)
	// Output: <nil> []
}
```

## Review

The backoff is correct when `Retry` sleeps `base<<i` after the i-th failure and
performs exactly `attempts-1` sleeps in the worst case — never after the final
attempt. The fixture is correct when its `t.Cleanup` turns an unmet sleep
expectation into a failure: `TestRetrySucceedsAfterFailures` passes only because
the code slept `[10ms, 20ms]` and nothing else, and if `Retry` regressed to sleep
after the successful attempt the cleanup would catch the extra `40ms`. That
automatic verification is the value over a fake that merely records and is never
checked. Run `go test -race`: `TestClockConcurrentNow` exercises the spy's mutex,
and no test consults the wall clock, so the whole suite is instant and
deterministic. The mistake to avoid is a fake with no expectation check — it lets
"the code never slept at all" pass silently.

## Resources

- [testing.T.Cleanup](https://pkg.go.dev/testing#T.Cleanup) — the post-test hook the expectation check rides on.
- [time.Duration](https://pkg.go.dev/time#Duration) — the sleep durations the backoff computes and the spy records.
- [Go Wiki: Table-driven tests and clocks](https://go.dev/wiki/TableDrivenTests) — patterns for injecting time into testable code.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../04-subtests-and-t-run/00-concepts.md](../04-subtests-and-t-run/00-concepts.md)
