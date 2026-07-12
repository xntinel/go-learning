# Exercise 22: DNS Resolver with Fallback Endpoint Chain

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A service that depends on DNS to find its dependencies cannot afford a single
point of failure in the resolution path: a primary resolver, a secondary, and
maybe a hardcoded fallback should all be tried in order, with a growing delay
between attempts so a flapping resolver isn't hammered in a tight loop. The
chain's length is not fixed, so `Resolve` takes any number of resolvers as a
variadic parameter — and because tests must never depend on a real clock or a
real network, both the sleep and the resolvers themselves are injected
functions.

## What you'll build

```text
dnschain/                   independent module: example.com/dnschain
  go.mod                    go 1.24
  dnschain.go               package dnschain; type Resolver func(...); type Chain; New(baseDelay, sleep); (*Chain).Resolve(ctx, host, resolvers ...Resolver)
  cmd/
    demo/
      main.go               runnable demo: two failing resolvers then a succeeding one, and a chain that fully fails
  dnschain_test.go          table tests: first success wins, exponential backoff sequence, no sleep before the first attempt, aggregated failure, zero resolvers, cancelled context
```

- Files: `dnschain.go`, `cmd/demo/main.go`, `dnschain_test.go`.
- Implement: `type Resolver func(ctx context.Context, host string) ([]string, error)`, `New(baseDelay time.Duration, sleep func(time.Duration)) *Chain`, and `(*Chain).Resolve(ctx context.Context, host string, resolvers ...Resolver) ([]string, error)` that tries each resolver in turn with exponential backoff between attempts.
- Test: the first resolver that returns a non-empty result wins and later resolvers are never called; the recorded sleep durations double each attempt (`baseDelay`, `2*baseDelay`, `4*baseDelay`, ...); no sleep happens before the very first attempt; if every resolver fails, the returned error mentions every failure; zero resolvers is an error; an already-cancelled context aborts before any resolver runs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/22-dns-resolver-fallback-chain/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/22-dns-resolver-fallback-chain
go mod edit -go=1.24
```

### An injected clock and injected resolvers, not `time.Sleep` and `net.Resolver`

`Chain.sleep` is a `func(time.Duration)` field, not a direct call to
`time.Sleep`, and `Resolver` is a function type a test can implement with a
fake instead of `(*net.Resolver).LookupHost`. Both choices exist for the same
reason: a test asserting "the delay doubles each attempt" cannot afford to
actually wait `10ms + 20ms + 40ms + ...` real milliseconds per test case, and
a test asserting "the chain falls through to the third resolver" cannot
depend on real DNS infrastructure being reachable, flaky, or even resolvable
in a sandboxed CI runner. Injecting both means `TestResolveBacksOffExponentially`
runs in microseconds and is exactly as deterministic whether it's run once or
ten thousand times in `-count=10000`.

`Resolve` checks `ctx.Err()` *before* every attempt, including the first —
not just once at the top of the function — because a long fallback chain
with several resolvers could have its context cancelled partway through
(a caller's own timeout firing while attempt two is sleeping, say), and the
chain must stop calling resolvers immediately rather than continuing to burn
through the remaining fallbacks after the caller has already given up.
Failures accumulate in an `errs` slice and are joined with `errors.Join` only
once every resolver has been tried, so a fully-failed chain reports every
attempt's error, not just the last one — the same "evaluate everything, then
report all failures" shape as the WHERE-predicate combinator in exercise 20.

Create `dnschain.go`:

```go
// dnschain.go
package dnschain

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Resolver looks up the IPs for host. Production code wraps something like
// (*net.Resolver).LookupHost; tests substitute deterministic fakes so no
// real network lookup ever happens in this exercise.
type Resolver func(ctx context.Context, host string) ([]string, error)

// Chain tries any number of resolvers in order, backing off exponentially
// between attempts. sleep is injected so callers (in particular tests) never
// depend on a real wall-clock wait.
type Chain struct {
	baseDelay time.Duration
	sleep     func(time.Duration)
}

// New returns a Chain whose delay between attempt i and i+1 is
// baseDelay*2^(i-1). A nil sleep defaults to time.Sleep.
func New(baseDelay time.Duration, sleep func(time.Duration)) *Chain {
	if sleep == nil {
		sleep = time.Sleep
	}
	return &Chain{baseDelay: baseDelay, sleep: sleep}
}

// Resolve tries each resolver in order until one returns a non-empty result.
// Before every attempt after the first, it sleeps for an exponentially
// growing delay; before every attempt (including the first) it checks
// ctx.Err() and aborts immediately if the context has already been
// cancelled or its deadline has passed. If every resolver fails, Resolve
// returns one joined error carrying every attempt's failure.
func (c *Chain) Resolve(ctx context.Context, host string, resolvers ...Resolver) ([]string, error) {
	if len(resolvers) == 0 {
		return nil, fmt.Errorf("dnschain: no resolvers given")
	}

	var errs []error
	for i, r := range resolvers {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("dnschain: aborted before attempt %d: %w", i, err)
		}
		if i > 0 {
			delay := c.baseDelay * time.Duration(1<<(i-1))
			c.sleep(delay)
		}

		ips, err := r(ctx, host)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("resolver %d: %w", i, err))
		case len(ips) == 0:
			errs = append(errs, fmt.Errorf("resolver %d: empty result", i))
		default:
			return ips, nil
		}
	}

	return nil, fmt.Errorf("dnschain: all %d resolver(s) failed for %q: %w", len(resolvers), host, errors.Join(errs...))
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/dnschain"
)

func failing(name string) dnschain.Resolver {
	return func(ctx context.Context, host string) ([]string, error) {
		return nil, fmt.Errorf("%s: no route", name)
	}
}

func succeeding(ips ...string) dnschain.Resolver {
	return func(ctx context.Context, host string) ([]string, error) {
		return ips, nil
	}
}

func main() {
	chain := dnschain.New(10*time.Millisecond, nil)

	ips, err := chain.Resolve(context.Background(), "svc.internal",
		failing("primary"),
		failing("secondary"),
		succeeding("10.0.0.5"),
	)
	fmt.Println(ips, err)

	_, err = chain.Resolve(context.Background(), "svc.internal",
		failing("primary"),
		failing("secondary"),
	)
	fmt.Println(err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[10.0.0.5] <nil>
dnschain: all 2 resolver(s) failed for "svc.internal": resolver 0: primary: no route
resolver 1: secondary: no route
```

### Tests

`TestResolveBacksOffExponentiallyBetweenAttempts` is the one that pins the
backoff schedule itself: with `baseDelay=10ms` and four resolvers where only
the last succeeds, the recorded sleeps must be exactly `[10ms, 20ms, 40ms]`
— three delays for four attempts, since there is no delay before the first.

Create `dnschain_test.go`:

```go
// dnschain_test.go
package dnschain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func failing(msg string) Resolver {
	return func(ctx context.Context, host string) ([]string, error) {
		return nil, errors.New(msg)
	}
}

func succeeding(ips ...string) Resolver {
	return func(ctx context.Context, host string) ([]string, error) {
		return ips, nil
	}
}

func TestResolveReturnsFirstSuccess(t *testing.T) {
	t.Parallel()

	var slept []time.Duration
	c := New(10*time.Millisecond, func(d time.Duration) { slept = append(slept, d) })

	ips, err := c.Resolve(context.Background(), "svc.internal",
		failing("primary down"),
		failing("secondary down"),
		succeeding("10.0.0.5"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"10.0.0.5"}; !reflect.DeepEqual(ips, want) {
		t.Fatalf("ips = %v, want %v", ips, want)
	}
}

func TestResolveBacksOffExponentiallyBetweenAttempts(t *testing.T) {
	t.Parallel()

	var slept []time.Duration
	c := New(10*time.Millisecond, func(d time.Duration) { slept = append(slept, d) })

	_, err := c.Resolve(context.Background(), "svc.internal",
		failing("a"), failing("b"), failing("c"), succeeding("10.0.0.5"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	if !reflect.DeepEqual(slept, want) {
		t.Fatalf("sleep sequence = %v, want %v", slept, want)
	}
}

func TestResolveNeverSleepsBeforeFirstAttempt(t *testing.T) {
	t.Parallel()

	var slept []time.Duration
	c := New(10*time.Millisecond, func(d time.Duration) { slept = append(slept, d) })

	_, _ = c.Resolve(context.Background(), "svc.internal", succeeding("10.0.0.5"))
	if len(slept) != 0 {
		t.Fatalf("sleep calls = %v, want none for an immediate success", slept)
	}
}

func TestResolveJoinsAllFailuresWhenEveryResolverFails(t *testing.T) {
	t.Parallel()

	c := New(0, func(time.Duration) {})
	_, err := c.Resolve(context.Background(), "svc.internal", failing("primary down"), failing("secondary down"))
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"primary down", "secondary down"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
}

func TestResolveWithZeroResolversIsAnError(t *testing.T) {
	t.Parallel()

	c := New(0, func(time.Duration) {})
	if _, err := c.Resolve(context.Background(), "svc.internal"); err == nil {
		t.Fatalf("Resolve with zero resolvers: error = nil, want an error")
	}
}

func TestResolveAbortsOnAlreadyCancelledContext(t *testing.T) {
	t.Parallel()

	called := false
	spy := func(ctx context.Context, host string) ([]string, error) {
		called = true
		return succeeding("10.0.0.5")(ctx, host)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := New(0, func(time.Duration) {})
	_, err := c.Resolve(ctx, "svc.internal", spy)
	if err == nil {
		t.Fatalf("expected an error for an already-cancelled context")
	}
	if called {
		t.Fatalf("resolver was called despite a cancelled context")
	}
}

func ExampleChain_Resolve() {
	c := New(0, func(time.Duration) {})
	ips, err := c.Resolve(context.Background(), "svc.internal", failing("primary down"), succeeding("10.0.0.5"))
	fmt.Println(ips, err)
	// Output: [10.0.0.5] <nil>
}
```

## Review

`Resolve` is correct when it stops at the first resolver that returns a
non-empty result, calling no resolver after that; the recorded delays
between attempts double every time starting from `baseDelay`, with no delay
before the very first attempt; a fully exhausted chain reports every
resolver's failure in one error; and a context that is already done before
`Resolve` even starts prevents any resolver from being called at all. The
senior point is dependency injection as a determinism tool, not just a
testability nicety: swapping `time.Sleep` for a recording closure and
`net.Resolver` for a table of canned responses is what turns "test a
network retry loop with backoff" from a multi-second, flaky integration test
into a sub-millisecond, always-reliable unit test. The mistake to avoid is
checking `ctx.Err()` only once before the loop — a long chain can easily
outlive its context partway through, and every attempt needs its own check.

## Resources

- [`context.Context`](https://pkg.go.dev/context#Context) — `Err` reports whether the context is done and why.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining every failed attempt's error into one.
- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-span-attribute-enricher-otel.md](21-span-attribute-enricher-otel.md) | Next: [23-rbac-permission-validator-roles.md](23-rbac-permission-validator-roles.md)
