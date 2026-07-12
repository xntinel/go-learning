# Exercise 23: Cascading Timeouts — Nested Defers with Context Cancellation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An RPC call that hops from client to gateway to backend, each layer
imposing its own timeout on top of whatever deadline it inherited, leaks a
`context.WithTimeout` timer at every layer that forgets to defer its
`cancel`. Worse, whichever hop's timeout is *tightest* is the one that
actually governs the whole call — not necessarily the innermost one — and a
handler that assumes "the backend's timeout is what fires" gets surprised
the day an intermediate gateway is configured with a shorter budget. This
module builds all three hops with their own deferred `cancel` and cleanup
record, and a table of cases that puts the binding timeout at each layer in
turn. The module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
rpcchain/                   independent module: example.com/cascading-timeout-nested-defers
  go.mod                     go 1.24
  rpcchain.go                HopLog, withHopTimeout, Chain(ctx, log, clientT, gatewayT, backendT, work)
  cmd/
    demo/
      main.go                runnable demo: all hops within budget, then gateway's tighter timeout binds
  rpcchain_test.go           table over which hop's timeout binds; concurrent-calls case under -race
```

- Files: `rpcchain.go`, `cmd/demo/main.go`, `rpcchain_test.go`.
- Implement: `HopLog` (`Record`, `Entries`), `withHopTimeout`, `Chain(ctx context.Context, log *HopLog, clientTimeout, gatewayTimeout, backendTimeout time.Duration, work func(context.Context) (string, error)) (string, error)`.
- Test: a table over which hop's timeout is tightest (backend, gateway, or client), plus a concurrent-calls case sharing one `HopLog`, run under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the tightest timeout wins, and why the cleanup cascades in call order

Each hop calls `context.WithTimeout(ctx, hopTimeout)` on the context it
received from its caller — never on a fresh background context. A child
context's deadline can only ever be *tighter* than its parent's, never
looser: `context.WithTimeout` computes `min(parent deadline, now +
hopTimeout)`, so a longer request from a nested hop can never resurrect a
parent's already-tighter deadline. That is why the test table exercises all
three positions — client, gateway, backend — as the binding timeout: the
tightest one always governs, and a chain built to "obviously" fail at the
backend can just as easily fail earlier, at the gateway, if that hop is
misconfigured with too short a budget.

Cleanup itself is not one function's LIFO defers here — it is three nested
function calls, each with its own `defer cancel()` and `defer
log.Record(...)`. That still cascades in a strict, provable order: the
innermost call (`backend`) returns first and runs its own defers first,
*then* control returns to `gateway`'s frame and its defers run, and only
after that does `client`'s frame return and run its defers. `HopLog` exists
specifically to make that cascade — backend, then gateway, then client —
visible and assertable, on both the success path and every timeout
scenario. `defer cancel()` at each layer matters even when nothing times
out: every `context.WithTimeout` starts an internal timer goroutine, and
skipping the deferred `cancel` leaks it until the parent context's own
deadline eventually fires — potentially much later, or never, if the parent
has no deadline at all.

Create `rpcchain.go`:

```go
package rpcchain

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// HopLog records which hops ran their cleanup, in the order they did, and
// is safe for concurrent use so many chained calls can share one log.
type HopLog struct {
	mu      sync.Mutex
	entries []string
}

// Record appends one cleanup entry.
func (l *HopLog) Record(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
}

// Entries returns a copy of everything recorded so far.
func (l *HopLog) Entries() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

// withHopTimeout wraps ctx with this hop's own timeout, defers releasing
// that timeout's resources and recording this hop's cleanup, then calls
// next with the tightened context. Any error next returns is wrapped with
// the hop's name so a caller can see which hop the failure surfaced at.
//
// defer cancel() runs whether next returns normally or an intermediate
// hop's timeout fired somewhere deeper in the chain -- context.WithTimeout
// always needs its cancel called to release the timer it started
// internally, on every path, or that timer leaks until the parent context
// itself is done.
func withHopTimeout(ctx context.Context, name string, timeout time.Duration, log *HopLog, next func(context.Context) (string, error)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer log.Record(name + " cleanup")

	result, err := next(ctx)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return result, nil
}

// Chain simulates a three-hop RPC call -- client, then gateway, then
// backend -- each imposing its own timeout on top of whatever deadline it
// inherited from the caller before it. context.WithTimeout never lets a
// child context outlive its parent's deadline, so whichever hop's timeout
// is tightest is the one that actually binds, regardless of which hop is
// "closest" to the operation that is slow.
//
// Each hop's withHopTimeout call defers its own cancel and cleanup-log
// entry. Because the hops are nested function calls, not nested defers in
// one frame, their cleanup naturally cascades in call-stack order: the
// innermost hop (backend) returns and runs its defers first, then control
// returns to gateway, which runs its own defers, then finally client. That
// cascade is what the entries slice on HopLog is built to prove.
func Chain(ctx context.Context, log *HopLog, clientTimeout, gatewayTimeout, backendTimeout time.Duration, work func(context.Context) (string, error)) (string, error) {
	return withHopTimeout(ctx, "client", clientTimeout, log, func(ctx context.Context) (string, error) {
		return withHopTimeout(ctx, "gateway", gatewayTimeout, log, func(ctx context.Context) (string, error) {
			return withHopTimeout(ctx, "backend", backendTimeout, log, work)
		})
	})
}
```

### The runnable demo

The demo runs the chain once with every hop's timeout well inside budget,
and once with the gateway's timeout tighter than the work would need,
printing the result, error, and cleanup cascade for each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/cascading-timeout-nested-defers"
)

// slowWork simulates the actual backend operation, honoring cancellation.
func slowWork(delay time.Duration) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		select {
		case <-time.After(delay):
			return "backend-result", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func main() {
	fmt.Println("-- all hops within budget --")
	log1 := &rpcchain.HopLog{}
	result, err := rpcchain.Chain(context.Background(), log1,
		500*time.Millisecond, 400*time.Millisecond, 300*time.Millisecond,
		slowWork(5*time.Millisecond))
	fmt.Println("result:", result, "error:", err)
	fmt.Println("cleanup order:", log1.Entries())

	fmt.Println("-- gateway's tighter timeout cuts the chain short --")
	log2 := &rpcchain.HopLog{}
	result, err = rpcchain.Chain(context.Background(), log2,
		500*time.Millisecond, 30*time.Millisecond, 300*time.Millisecond,
		slowWork(200*time.Millisecond))
	fmt.Println("result:", result, "error:", err)
	fmt.Println("cleanup order:", log2.Entries())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
-- all hops within budget --
result: backend-result error: <nil>
cleanup order: [backend cleanup gateway cleanup client cleanup]
-- gateway's tighter timeout cuts the chain short --
result:  error: client: gateway: backend: context deadline exceeded
cleanup order: [backend cleanup gateway cleanup client cleanup]
```

### Tests

`TestChainTimeoutCascade` runs a table where the binding timeout is placed
at each of the three hops in turn — backend, gateway, and client — plus a
success case, asserting the wrapped error chain via `errors.Is` and that
`HopLog.Entries()` always shows the same backend-then-gateway-then-client
cascade regardless of which hop actually timed out.
`TestChainConcurrentCallsShareLogSafely` runs twenty concurrent `Chain`
calls against one shared `HopLog` to prove it is race-free and that every
call still records its own complete three-entry cascade.

Create `rpcchain_test.go`:

```go
package rpcchain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func slowWork(delay time.Duration) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		select {
		case <-time.After(delay):
			return "backend-result", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func TestChainTimeoutCascade(t *testing.T) {
	tests := []struct {
		name           string
		clientTimeout  time.Duration
		gatewayTimeout time.Duration
		backendTimeout time.Duration
		workDelay      time.Duration
		wantErr        bool
		wantErrPrefix  string
	}{
		{
			name:           "all hops within budget",
			clientTimeout:  500 * time.Millisecond,
			gatewayTimeout: 400 * time.Millisecond,
			backendTimeout: 300 * time.Millisecond,
			workDelay:      5 * time.Millisecond,
			wantErr:        false,
		},
		{
			name:           "gateway's tighter timeout is the binding one",
			clientTimeout:  500 * time.Millisecond,
			gatewayTimeout: 30 * time.Millisecond,
			backendTimeout: 300 * time.Millisecond,
			workDelay:      200 * time.Millisecond,
			wantErr:        true,
			wantErrPrefix:  "client: gateway: backend:",
		},
		{
			name:           "backend's own timeout is the binding one",
			clientTimeout:  500 * time.Millisecond,
			gatewayTimeout: 400 * time.Millisecond,
			backendTimeout: 30 * time.Millisecond,
			workDelay:      200 * time.Millisecond,
			wantErr:        true,
			wantErrPrefix:  "client: gateway: backend:",
		},
		{
			name:           "client's outer timeout binds despite looser nested requests",
			clientTimeout:  30 * time.Millisecond,
			gatewayTimeout: 400 * time.Millisecond,
			backendTimeout: 300 * time.Millisecond,
			workDelay:      200 * time.Millisecond,
			wantErr:        true,
			wantErrPrefix:  "client: gateway: backend:",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			log := &HopLog{}

			result, err := Chain(context.Background(), log,
				tc.clientTimeout, tc.gatewayTimeout, tc.backendTimeout,
				slowWork(tc.workDelay))

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got result %q", result)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
				}
				if !strings.HasPrefix(err.Error(), tc.wantErrPrefix) {
					t.Errorf("err = %q, want prefix %q", err.Error(), tc.wantErrPrefix)
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}

			wantEntries := []string{"backend cleanup", "gateway cleanup", "client cleanup"}
			if entries := log.Entries(); !equalSlices(entries, wantEntries) {
				t.Errorf("cleanup order = %v, want %v", entries, wantEntries)
			}
		})
	}
}

// TestChainConcurrentCallsShareLogSafely runs many Chain calls concurrently
// against one shared HopLog to prove -race sees no data race on it and that
// every call still records its own complete cascade of three cleanup
// entries, regardless of how the calls interleave.
func TestChainConcurrentCallsShareLogSafely(t *testing.T) {
	const calls = 20
	log := &HopLog{}

	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = Chain(context.Background(), log,
				200*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond,
				slowWork(time.Millisecond))
		}()
	}
	wg.Wait()

	entries := log.Entries()
	if len(entries) != calls*3 {
		t.Fatalf("recorded %d entries, want %d", len(entries), calls*3)
	}
	counts := map[string]int{}
	for _, e := range entries {
		counts[e]++
	}
	for _, name := range []string{"backend cleanup", "gateway cleanup", "client cleanup"} {
		if counts[name] != calls {
			t.Errorf("count[%q] = %d, want %d", name, counts[name], calls)
		}
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

Verify: `go test -count=1 -race ./...`

## Review

The chain is correct when two things both hold regardless of which hop's
timeout is tightest: the error the caller sees traces back to
`context.DeadlineExceeded` through every layer's wrapping, and the cleanup
cascade always runs backend-then-gateway-then-client, matching the call
stack's actual unwind order. Both properties come from giving every hop the
same shape — wrap, defer cancel, defer log, call inward — rather than
special-casing "the backend is where timeouts happen." The mistake this
design avoids is assuming the innermost hop's timeout is always what fires;
a chain that only tests the backend timing out never notices a gateway
misconfigured with too tight a budget until it happens in production, one
layer up from where anyone was looking.

## Resources

- [context package](https://pkg.go.dev/context) — `WithTimeout`'s parent-deadline-wins contract and the required `cancel` call.
- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — deferred functions execute in LIFO order at their own frame's return.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — deadline propagation across a call chain.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-panic-recovery-deferred-cleanup.md](22-panic-recovery-deferred-cleanup.md) | Next: [24-wal-deferred-flush-named-return.md](24-wal-deferred-flush-named-return.md)
