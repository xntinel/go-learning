# Exercise 3: A Pluggable Limiter Port for Swappable Backends

Your service has two limiter implementations with identical observable
behavior and different internals — and a third (a library adapter) is coming
in Exercise 8. This exercise turns `Allow() bool` into a proper port:
compile-time satisfaction checks that fail the build on drift, and a shared
conformance suite that runs the identical behavioral contract against every
backend, so adding one costs zero new test logic.

## What you'll build

```text
limiterport/                    independent module: example.com/limiterport
  go.mod
  limiter/
    limiter.go                  type Limiter interface; compile-time var _ checks
    mutex.go                    MutexLimiter (continuous refill)
    channel.go                  ChannelLimiter (ticker refill, Close)
    conformance_test.go         testLimiterContract run against both via t.Run
  cmd/
    demo/
      main.go                   same load through the port, both backends
```

- Files: `limiter/limiter.go`, `limiter/mutex.go`, `limiter/channel.go`, `limiter/conformance_test.go`, `cmd/demo/main.go`.
- Implement: the `Limiter` interface, `var _ Limiter = (*T)(nil)` assertions for both implementations, and a `testLimiterContract` helper parameterized over a constructor.
- Test: a table of named constructors feeding the shared contract (burst, deny, concurrent exact-N) through `t.Run` subtests, plus an `Example` documenting the port's behavior.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/10-mutex-vs-channel/03-limiter-interface/limiter go-solutions/15-sync-primitives/10-mutex-vs-channel/03-limiter-interface/cmd/demo
cd go-solutions/15-sync-primitives/10-mutex-vs-channel/03-limiter-interface
```

### The port: one method, defined by the consumer

Go interfaces belong to the code that *consumes* them, and the consumer here
— an HTTP middleware, a job dispatcher — needs exactly one capability: "may
this operation proceed right now?". So the port is one method:

```
type Limiter interface {
	Allow() bool
}
```

Resist the urge to widen it. `Close` is deliberately absent: only the channel
implementation has a lifecycle, and forcing a no-op `Close` onto the mutex
version (or worse, `Tokens()` onto implementations that cannot report
fractions) turns a clean port into a lowest-common-denominator grab bag.
Callers that own a `ChannelLimiter` close it where they constructed it —
construction and teardown live together; the port is only the admission
surface. This is the interface-segregation instinct applied to concurrency
tooling.

### Compile-time checks fail the build, not the test

```
var (
	_ Limiter = (*MutexLimiter)(nil)
	_ Limiter = (*ChannelLimiter)(nil)
)
```

Each line asks the compiler to convert a typed nil pointer to the interface;
if a method is renamed or a signature drifts (`Allow(ctx)` in a refactor),
compilation fails at this line, in this file, naming the offending type. The
cost is zero bytes at runtime — the blank identifier discards the value. Put
these next to the interface (or next to each implementation); do not bury the
same knowledge in a test that someone can skip. The original version of this
lesson checked interface satisfaction inside a `TestLimiterInterface` test
body — strictly weaker, because it only fails when tests run, and it reports
a test failure instead of pointing at the drifted declaration.

### The conformance suite: the contract is the test

With two implementations, copy-pasting burst/deny/storm tests per backend is
already a smell; with three it is unmaintainable drift waiting to happen. The
production pattern is a *conformance function*: a test helper parameterized
over a constructor, run under `t.Run` per named backend. The constructor
returns a cleanup function so lifecycle-owning backends can release their
goroutine — for the mutex limiter the cleanup is a no-op. Each backend is
constructed with refill disabled (rate 0, or an interval of `time.Hour`) so
the contract asserts *exact* counts.

Adding Exercise 8's `rate.Limiter` adapter to this suite will be one table
entry. That is the payoff.

Create `limiter/limiter.go`:

```go
// Package limiter defines the admission port and two interchangeable
// token-bucket implementations behind it.
package limiter

// Limiter is the admission port consumed by middleware and dispatchers:
// Allow reports whether one operation may proceed right now, spending one
// token if so. Implementations must be safe for concurrent use.
type Limiter interface {
	Allow() bool
}

// Compile-time proof that every implementation satisfies the port. A drifted
// method signature fails the build here, naming the offending type.
var (
	_ Limiter = (*MutexLimiter)(nil)
	_ Limiter = (*ChannelLimiter)(nil)
)
```

Create `limiter/mutex.go`:

```go
package limiter

import (
	"sync"
	"time"
)

// MutexLimiter is a token bucket with continuous elapsed-time refill,
// guarded by a single mutex. It is a passive value: no goroutine, no Close.
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
```

Create `limiter/channel.go`:

```go
package limiter

import (
	"sync"
	"time"
)

// ChannelLimiter is a token bucket over a pre-filled buffered channel with a
// ticker-driven refill goroutine. Callers must Close it to stop the goroutine.
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

// Close stops the refill goroutine. Idempotent and safe from any goroutine.
func (cl *ChannelLimiter) Close() {
	cl.once.Do(func() { close(cl.stop) })
}
```

### The demo: one loop, two backends, one port

The demo's load loop takes the interface, not a concrete type — the proof
that call sites are backend-agnostic. Both limiters are configured with
refill disabled, so the counts are deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/limiterport/limiter"
)

func drain(l limiter.Limiter, n int) int {
	allowed := 0
	for range n {
		if l.Allow() {
			allowed++
		}
	}
	return allowed
}

func main() {
	ml := limiter.NewMutexLimiter(5, 0)
	cl := limiter.NewChannelLimiter(5, time.Hour)
	defer cl.Close()

	fmt.Printf("mutex  : allowed %d/8\n", drain(ml, 8))
	fmt.Printf("channel: allowed %d/8\n", drain(cl, 8))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
mutex  : allowed 5/8
channel: allowed 5/8
```

### The conformance test

`newLimiter` returns the port plus a cleanup func; the table names each
backend; every property runs as a `t.Run` subtest so failures read
`TestLimiterConformance/channel/exact_burst_under_concurrency`. Outer and
per-backend subtests are parallel — each constructs its own limiter, so
there is no shared state to collide on.

Create `limiter/conformance_test.go`:

```go
package limiter

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newLimiter builds a backend with the given burst and refill disabled, plus
// a cleanup func (a no-op for backends without a lifecycle).
type newLimiter func(burst int) (Limiter, func())

func conformanceBackends() map[string]newLimiter {
	return map[string]newLimiter{
		"mutex": func(burst int) (Limiter, func()) {
			return NewMutexLimiter(float64(burst), 0), func() {}
		},
		"channel": func(burst int) (Limiter, func()) {
			cl := NewChannelLimiter(burst, time.Hour)
			return cl, cl.Close
		},
	}
}

// testLimiterContract is the shared behavioral contract. Every backend —
// including ones added later — must pass it unchanged.
func testLimiterContract(t *testing.T, construct newLimiter) {
	t.Helper()

	t.Run("burst then deny", func(t *testing.T) {
		t.Parallel()
		l, cleanup := construct(4)
		defer cleanup()

		for i := range 4 {
			if !l.Allow() {
				t.Fatalf("Allow #%d denied during burst", i)
			}
		}
		if l.Allow() {
			t.Fatal("Allow succeeded on an empty bucket")
		}
	})

	t.Run("exact burst under concurrency", func(t *testing.T) {
		t.Parallel()
		l, cleanup := construct(300)
		defer cleanup()

		var allowed atomic.Int64
		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() {
				for range 20 {
					if l.Allow() {
						allowed.Add(1)
					}
				}
			})
		}
		wg.Wait()

		if got, want := allowed.Load(), int64(300); got != want {
			t.Fatalf("allowed = %d, want exactly %d", got, want)
		}
	})
}

func TestLimiterConformance(t *testing.T) {
	t.Parallel()
	for name, construct := range conformanceBackends() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testLimiterContract(t, construct)
		})
	}
}

func ExampleLimiter() {
	var l Limiter = NewMutexLimiter(2, 0)
	fmt.Println(l.Allow(), l.Allow(), l.Allow())
	// Output: true true false
}
```

## Review

The two failure modes this module eliminates are drift and divergence. Drift
— an implementation's signature wandering away from the port — is caught at
compile time by the `var _` lines, which is as early as an error can be
caught. Divergence — two backends passing their own slightly different test
suites while behaving differently — is impossible when there is exactly one
contract and every backend runs it verbatim. When you extend the contract
(say, "Allow must be safe after cleanup"), every backend is held to the new
rule in the same commit.

Keep the port one method wide, and notice how the cleanup-func pattern let a
lifecycle-owning backend and a passive one share a constructor signature
without polluting the interface. Confirm with `go test -count=1 -race ./...`
— the per-backend subtest names in verbose output are the readable inventory
of what the port guarantees.

## Resources

- [Effective Go: interfaces](https://go.dev/doc/effective_go#interfaces) — small interfaces, satisfaction without declaration.
- [Go Code Review Comments: interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — define interfaces on the consumer side.
- [testing package: subtests](https://pkg.go.dev/testing#hdr-Subtests_and_Sub_benchmarks) — `t.Run` naming and parallelism rules the conformance suite relies on.

---

Prev: [02-channel-semaphore-limiter.md](02-channel-semaphore-limiter.md) | Back to [00-concepts.md](00-concepts.md) | Next: [04-limiter-demo-cli.md](04-limiter-demo-cli.md)
