# Exercise 4: Load-Comparison CLI: Same Traffic, Two Limiters

Before your team picks a limiter implementation for the gateway, someone asks
the obvious question: "do they actually behave the same under our traffic
shape?" This exercise builds the tool that answers it — a CLI that drives
both implementations through an identical request loop and prints the
allowed/denied split, with flags for burst, rate, request count, and
inter-request pacing so the continuous-vs-ticker refill difference becomes
observable instead of theoretical.

## What you'll build

```text
limitercli/                     independent module: example.com/limitercli
  go.mod
  harness/
    harness.go                  Limiter port, both implementations,
                                Result, RunLoad (the testable core)
    harness_test.go             count-invariant tests + the divergence test
  cmd/
    demo/
      main.go                   flag wiring and printing only
```

- Files: `harness/harness.go`, `harness/harness_test.go`, `cmd/demo/main.go`.
- Implement: `RunLoad(l Limiter, n int, delay time.Duration) Result` as a pure, testable function; `main` only parses flags, constructs limiters, and prints.
- Test: allowed+denied always equals n; exact burst with refill off; a divergence test proving the mutex limiter refills during a paced loop while the hour-interval channel limiter does not.
- Verify: `go test -count=1 -race ./...`, `go vet ./...`, and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/10-mutex-vs-channel/04-limiter-demo-cli/harness go-solutions/15-sync-primitives/10-mutex-vs-channel/04-limiter-demo-cli/cmd/demo
cd go-solutions/15-sync-primitives/10-mutex-vs-channel/04-limiter-demo-cli
```

### Keep main() too thin to need testing

The design rule for every CLI you ship: `main` is glue — flags in, function
call, `Printf` out — and everything with behavior lives in a package a test
can import. The original version of this demo had the load loop inline in
`main`, which meant its only verification was eyeballing stdout. Moving the
loop into `harness.RunLoad` returning a `Result` struct makes the same code
unit-testable, benchmarkable, and reusable by the comparison test below.
This split is not cosmetic: a demo `main` cannot fail CI when the loop
regresses; a test on `RunLoad` can.

`RunLoad` itself is deliberately boring — call `Allow`, count both outcomes,
optionally sleep between requests. The `delay` parameter is what makes the
tool diagnostic: with `delay=0` the whole loop finishes in microseconds, no
refill lands in either implementation, and both print exactly the burst.
With a real delay, elapsed time accumulates and the two refill strategies
split: the mutex limiter credits fractional tokens continuously during the
pacing, while a channel limiter only gains whole tokens when its ticker
fires. Running the tool at different `-delay` values is how you *see* the
semantic difference the previous exercises described.

Create `harness/harness.go`:

```go
// Package harness drives interchangeable rate limiters through an identical
// load loop so their admission behavior can be compared empirically.
package harness

import (
	"sync"
	"time"
)

// Limiter is the admission port both implementations satisfy.
type Limiter interface {
	Allow() bool
}

var (
	_ Limiter = (*MutexLimiter)(nil)
	_ Limiter = (*ChannelLimiter)(nil)
)

// Result is the outcome of one load run.
type Result struct {
	Allowed int
	Denied  int
}

// RunLoad sends n admission requests through l, pausing delay between
// requests. It is the testable core: main only wires flags around it.
func RunLoad(l Limiter, n int, delay time.Duration) Result {
	var res Result
	for range n {
		if l.Allow() {
			res.Allowed++
		} else {
			res.Denied++
		}
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	return res
}

// MutexLimiter: continuous elapsed-time refill under one mutex.
type MutexLimiter struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second; 0 disables refill
	lastRefill time.Time
}

func NewMutexLimiter(maxTokens, refillRate float64) *MutexLimiter {
	return &MutexLimiter{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

func (l *MutexLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	l.tokens += now.Sub(l.lastRefill).Seconds() * l.refillRate
	if l.tokens > l.maxTokens {
		l.tokens = l.maxTokens
	}
	l.lastRefill = now

	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}

// ChannelLimiter: pre-filled buffered channel, one token per ticker tick.
type ChannelLimiter struct {
	tokens chan struct{}
	stop   chan struct{}
	once   sync.Once
}

func NewChannelLimiter(maxTokens int, refillInterval time.Duration) *ChannelLimiter {
	cl := &ChannelLimiter{
		tokens: make(chan struct{}, maxTokens),
		stop:   make(chan struct{}),
	}
	for range maxTokens {
		cl.tokens <- struct{}{}
	}
	go func() {
		ticker := time.NewTicker(refillInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case cl.tokens <- struct{}{}:
				default:
				}
			case <-cl.stop:
				return
			}
		}
	}()
	return cl
}

func (cl *ChannelLimiter) Allow() bool {
	select {
	case <-cl.tokens:
		return true
	default:
		return false
	}
}

// Close stops the refill goroutine. Idempotent.
func (cl *ChannelLimiter) Close() {
	cl.once.Do(func() { close(cl.stop) })
}
```

### The CLI: flags for the knobs an engineer actually turns

`flag.Duration` parses human-friendly values (`-delay 2ms`, `-interval
100ms`) directly into `time.Duration` — no hand-rolled parsing. The default
configuration uses `delay=0`, which makes the default run deterministic
(both limiters admit exactly the burst) and therefore usable as a smoke
check. The `defer cl.Close()` is the lifecycle discipline from Exercise 2:
even a short-lived CLI models the shutdown path correctly, because tools
grow into daemons.

Create `cmd/demo/main.go`:

```go
package main

import (
	"flag"
	"fmt"

	"example.com/limitercli/harness"
)

func main() {
	var (
		burst    = flag.Int("burst", 10, "bucket capacity for both limiters")
		rate     = flag.Float64("rate", 100, "mutex limiter refill (tokens/second)")
		interval = flag.Duration("interval", 0, "channel limiter refill interval (0 = off)")
		n        = flag.Int("n", 50, "number of admission requests per limiter")
		delay    = flag.Duration("delay", 0, "pause between requests")
	)
	flag.Parse()

	chanInterval := *interval
	if chanInterval <= 0 {
		// A zero interval panics time.NewTicker; treat 0 as refill-off,
		// mirroring rate=0 on the mutex side.
		chanInterval = 1 << 62
	}

	ml := harness.NewMutexLimiter(float64(*burst), *rate)
	cl := harness.NewChannelLimiter(*burst, chanInterval)
	defer cl.Close()

	fmt.Printf("requests=%d burst=%d rate=%g/s interval=%v delay=%v\n",
		*n, *burst, *rate, *interval, *delay)

	mres := harness.RunLoad(ml, *n, *delay)
	cres := harness.RunLoad(cl, *n, *delay)
	fmt.Printf("mutex  : allowed=%d denied=%d\n", mres.Allowed, mres.Denied)
	fmt.Printf("channel: allowed=%d denied=%d\n", cres.Allowed, cres.Denied)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests=50 burst=10 rate=100/s interval=0s delay=0s
mutex  : allowed=10 denied=40
channel: allowed=10 denied=40
```

With `delay=0` the fifty `Allow` calls complete in microseconds — at 100
tokens/s the mutex limiter accrues far less than one token, and no ticker
could fire — so both admit exactly the burst of 10. Now make the refill
difference visible:

```bash
go run ./cmd/demo -burst 5 -rate 100 -interval 100ms -n 20 -delay 5ms
```

The run takes ~100ms of paced time. The mutex limiter refills continuously
(~10 extra tokens at 100/s) and admits most of the 20 requests; the channel
limiter gains only one whole token per 100ms tick and admits about 6. Same
policy parameters on paper, different admission behavior — this asymmetry is
the reason the comparison harness exists. (These paced numbers vary slightly
run to run; the deterministic default run above is the one to compare
byte-for-byte.)

### Tests: invariants first, then the divergence

The count invariant (`Allowed + Denied == n`) and the exact-burst run are
table tests over both backends. `TestRefillDivergence` is the interesting
one: it runs a paced loop against a mutex limiter refilling at 100/s and a
channel limiter whose ticker will not fire for an hour. The channel side
must admit *exactly* its burst — a hard assertion, since no tick can land.
The mutex side must admit *more than* its burst — also safe, because
`time.Sleep(5ms)` sleeps at least 5ms, so 19 pauses guarantee at least 95ms
of elapsed time and therefore at least 9 whole refilled tokens. The test
asserts a guaranteed lower bound, not a flaky exact value.

Create `harness/harness_test.go`:

```go
package harness

import (
	"fmt"
	"testing"
	"time"
)

func TestRunLoadCountsAreExhaustive(t *testing.T) {
	t.Parallel()

	backends := map[string]func() (Limiter, func()){
		"mutex": func() (Limiter, func()) {
			return NewMutexLimiter(7, 0), func() {}
		},
		"channel": func() (Limiter, func()) {
			cl := NewChannelLimiter(7, time.Hour)
			return cl, cl.Close
		},
	}
	for name, construct := range backends {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l, cleanup := construct()
			defer cleanup()

			res := RunLoad(l, 25, 0)
			if res.Allowed+res.Denied != 25 {
				t.Fatalf("Allowed+Denied = %d, want 25", res.Allowed+res.Denied)
			}
			if res.Allowed != 7 {
				t.Fatalf("Allowed = %d, want exactly the burst of 7", res.Allowed)
			}
		})
	}
}

func TestRefillDivergence(t *testing.T) {
	t.Parallel()

	const (
		burst = 5
		n     = 20
		delay = 5 * time.Millisecond
	)

	ml := NewMutexLimiter(burst, 100) // 100 tokens/s: ~0.5 token per pause
	cl := NewChannelLimiter(burst, time.Hour)
	defer cl.Close()

	mres := RunLoad(ml, n, delay)
	cres := RunLoad(cl, n, delay)

	// The channel limiter cannot gain a token for an hour: exactly burst.
	if cres.Allowed != burst {
		t.Fatalf("channel Allowed = %d, want exactly %d", cres.Allowed, burst)
	}
	// The mutex limiter refills continuously during the paced loop. Sleep
	// guarantees at least 19*5ms = 95ms elapsed, i.e. at least 9 extra
	// tokens: assert the guaranteed lower bound, never an exact flaky count.
	if mres.Allowed < burst+9 {
		t.Fatalf("mutex Allowed = %d, want at least %d (continuous refill)", mres.Allowed, burst+9)
	}
}

func ExampleRunLoad() {
	l := NewMutexLimiter(3, 0)
	res := RunLoad(l, 10, 0)
	fmt.Printf("allowed=%d denied=%d\n", res.Allowed, res.Denied)
	// Output: allowed=3 denied=7
}
```

## Review

Two traps live in this module. The first is `time.NewTicker(0)`, which
panics: a CLI whose flag defaults to "refill off" must translate that into a
value the ticker accepts — here an absurdly long interval — rather than
passing zero through. The second is asserting exact counts on a wall-clock
paced run: `TestRefillDivergence` deliberately asserts an exact count only
where no refill can occur, and a *lower bound* where refill is continuous,
because `time.Sleep` guarantees a minimum, never a maximum. If you find
yourself writing `want 14` against a real-time loop, the test is already
flaky; re-derive what the sleep actually guarantees.

Confirm with `go vet ./...` and `go test -count=1 -race ./...`, then run the
two demo invocations and watch the divergence with your own eyes — that
observed gap between "same parameters" and "same behavior" is the single
most useful thing to carry into a design review.

## Resources

- [flag package](https://pkg.go.dev/flag) — `flag.Int`, `flag.Float64`, `flag.Duration`, and parsing rules.
- [time package: NewTicker](https://pkg.go.dev/time#NewTicker) — including the panic on non-positive intervals.
- [Go wiki: Use a sync.Mutex or a channel?](https://go.dev/wiki/MutexOrChannel) — the decision this harness makes empirical.

---

Prev: [03-limiter-interface.md](03-limiter-interface.md) | Back to [00-concepts.md](00-concepts.md) | Next: [05-concurrency-contract-tests.md](05-concurrency-contract-tests.md)
