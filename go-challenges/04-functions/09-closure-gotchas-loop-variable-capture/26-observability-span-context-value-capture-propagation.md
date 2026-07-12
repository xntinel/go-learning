# Exercise 26: Observability Span Setup: Loop Context Values Captured in RPC Callbacks

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A dispatcher fans out one outbound RPC per service, and each RPC reads a
telemetry attribute back out of its `context.Context` to label its span. The
trap is not about the loop's range variable at all: it is about storing ONE
mutable value behind `context.WithValue` and mutating it in place every
iteration, instead of giving each RPC its own value. `context.Context` is
supposed to be immutable, but nothing stops you from stuffing a pointer to a
mutable struct inside it — and if you do, every context derived from that
one `context.WithValue` call shares the SAME mutable memory.

## What you'll build

```text
spanctx/                     independent module: example.com/spanctx
  go.mod                     go 1.24
  spanctx.go                 Attrs, Result, BuggyDispatch, FixedDispatch
  cmd/
    demo/
      main.go                runnable demo: dispatch to 3 services, print requested vs seen
  spanctx_test.go            table test: fixed isolates services, buggy collapses; edge case
```

- Files: `spanctx.go`, `cmd/demo/main.go`, `spanctx_test.go`.
- Implement: `BuggyDispatch` sharing one `*Attrs` behind `context.WithValue`, mutated once per loop iteration; `FixedDispatch` giving each RPC its own `*Attrs` and its own derived context.
- Test: dispatch concurrently to several services with a barrier so the buggy collapse is deterministic; assert fixed reports each RPC's own service, buggy reports every RPC seeing the last service; `-race` clean.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why a shared pointer behind a context value is the real bug here

`context.WithValue(ctx, key, value)` returns a NEW `context.Context` wrapping
`value`, but it does not copy `value` itself. If `value` is a pointer to a
mutable struct, every context derived from that call — and every goroutine
holding any of those contexts — reads through to the SAME memory. `Buggy
Dispatch` builds one `*Attrs`, wraps it once with `context.WithValue`, and
then mutates `shared.Service` on every loop iteration before dispatching that
iteration's RPC. A barrier (`ready`) makes the worst case deterministic:
every goroutine waits until the dispatch loop has finished mutating `shared`
for every service before reading it, so every goroutine reads the exact same,
final, service name — not the one it was dispatched for. This is a logic
bug, not a data race: `close(ready)` establishes happens-before between every
write to `shared.Service` and every goroutine's read, so `go test -race`
stays clean; the memory really is shared on purpose.

`FixedDispatch` gives each service its own `*Attrs` and its own context
built with a fresh `context.WithValue` call BEFORE spawning that service's
goroutine, so there is no shared memory to alias in the first place.

Create `spanctx.go`:

```go
package spanctx

import (
	"context"
	"sort"
	"sync"
)

type attrsKey struct{}

// Attrs is the telemetry attribute set an outbound RPC reads back out of its
// context to label its span.
type Attrs struct {
	Service string
}

func contextAttrs(ctx context.Context) *Attrs {
	a, _ := ctx.Value(attrsKey{}).(*Attrs)
	return a
}

// Result records, for one dispatched service, which service the RPC's own
// context reported when it actually ran.
type Result struct {
	Requested string
	Seen      string
}

// RPCFunc is an outbound call made with a per-dispatch context.
type RPCFunc func(ctx context.Context, service string)

// BuggyDispatch fans out one RPC per service, but stores a SINGLE shared
// *Attrs behind the context value and mutates that ONE struct in place on
// every loop iteration, instead of giving each RPC its own. Every goroutine
// waits on `ready` before reading the context, and `ready` is only closed
// after the dispatch loop has finished mutating `shared` for every service,
// so every goroutine deterministically observes the LAST service's name --
// not the one it was actually dispatched for. The mutation and the reads are
// both mutex-protected inside Attrs itself, so this is a logic bug, not a
// data race.
func BuggyDispatch(ctx context.Context, services []string, rpc RPCFunc) []Result {
	shared := &Attrs{}
	ctx = context.WithValue(ctx, attrsKey{}, shared)

	var (
		mu      sync.Mutex
		results []Result
		wg      sync.WaitGroup
	)
	ready := make(chan struct{})
	for _, svc := range services {
		shared.Service = svc // BUG: mutates the ONE shared Attrs every iteration
		wg.Add(1)
		go func(svc string) {
			defer wg.Done()
			<-ready // wait for dispatch to finish mutating shared
			rpc(ctx, svc)
			mu.Lock()
			results = append(results, Result{Requested: svc, Seen: contextAttrs(ctx).Service})
			mu.Unlock()
		}(svc)
	}
	close(ready) // only now do goroutines read the context; all see the final value
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].Requested < results[j].Requested })
	return results
}

// FixedDispatch fans out one RPC per service too, but builds a fresh *Attrs
// and a fresh derived context for each service BEFORE spawning its
// goroutine, so concurrent RPCs never alias the same telemetry value.
func FixedDispatch(ctx context.Context, services []string, rpc RPCFunc) []Result {
	var (
		mu      sync.Mutex
		results []Result
		wg      sync.WaitGroup
	)
	ready := make(chan struct{})
	for _, svc := range services {
		perCall := &Attrs{Service: svc}
		perCallCtx := context.WithValue(ctx, attrsKey{}, perCall)
		wg.Add(1)
		go func(svc string, ctx context.Context) {
			defer wg.Done()
			<-ready
			rpc(ctx, svc)
			mu.Lock()
			results = append(results, Result{Requested: svc, Seen: contextAttrs(ctx).Service})
			mu.Unlock()
		}(svc, perCallCtx)
	}
	close(ready)
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].Requested < results[j].Requested })
	return results
}
```

### The runnable demo

The demo dispatches to three services with both variants and prints what
each RPC actually saw in its context.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/spanctx"
)

func noop(ctx context.Context, service string) {}

func main() {
	services := []string{"billing", "inventory", "shipping"}

	buggy := spanctx.BuggyDispatch(context.Background(), services, noop)
	fmt.Println("buggy  results:")
	for _, r := range buggy {
		fmt.Printf("  requested=%-10s seen=%s\n", r.Requested, r.Seen)
	}

	fixed := spanctx.FixedDispatch(context.Background(), services, noop)
	fmt.Println("fixed  results:")
	for _, r := range fixed {
		fmt.Printf("  requested=%-10s seen=%s\n", r.Requested, r.Seen)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy  results:
  requested=billing    seen=shipping
  requested=inventory  seen=shipping
  requested=shipping   seen=shipping
fixed  results:
  requested=billing    seen=billing
  requested=inventory  seen=inventory
  requested=shipping   seen=shipping
```

### Tests

`TestDispatch` is a table test asserting `FixedDispatch` reports each RPC's
own service while `BuggyDispatch` reports every RPC seeing the last service.
`TestFixedDispatchSingleServiceEdgeCase` covers the boundary where there is
no other service to alias into.

Create `spanctx_test.go`:

```go
package spanctx

import (
	"context"
	"testing"
)

func noop(context.Context, string) {}

func TestDispatch(t *testing.T) {
	services := []string{"billing", "inventory", "shipping"}
	last := services[len(services)-1]

	tests := []struct {
		name     string
		dispatch func(context.Context, []string, RPCFunc) []Result
		want     []Result
	}{
		{
			name:     "fixed: each RPC sees its own service in its context",
			dispatch: FixedDispatch,
			want: []Result{
				{Requested: "billing", Seen: "billing"},
				{Requested: "inventory", Seen: "inventory"},
				{Requested: "shipping", Seen: "shipping"},
			},
		},
		{
			name:     "buggy: every RPC shares the one mutated Attrs and sees the last service",
			dispatch: BuggyDispatch,
			want: []Result{
				{Requested: "billing", Seen: last},
				{Requested: "inventory", Seen: last},
				{Requested: "shipping", Seen: last},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.dispatch(context.Background(), services, noop)
			if len(got) != len(tt.want) {
				t.Fatalf("results = %+v, want %+v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("results[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestFixedDispatchSingleServiceEdgeCase(t *testing.T) {
	t.Parallel()

	got := FixedDispatch(context.Background(), []string{"solo"}, noop)
	want := []Result{{Requested: "solo", Seen: "solo"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
}
```

## Review

Dispatch is correct when every RPC's context reports exactly the service it
was dispatched for, no matter how many RPCs run concurrently.
`context.WithValue` is often assumed to make a context value "safe" simply
because contexts are conventionally treated as immutable — but that
convention only holds if the VALUE you attach is itself immutable or is
never shared. `BuggyDispatch` attaches a pointer and then keeps mutating what
it points to, so every context derived from that one `WithValue` call reads
whatever the loop last wrote, regardless of which RPC actually holds that
context. The fix is not deeper synchronization; it is giving each RPC its
own value to attach in the first place. Run `go test -race` — the barrier
makes the collapse deterministic without ever creating an actual data race.

## Resources

- [`context.WithValue`](https://pkg.go.dev/context#WithValue) — attaches a value to a context; does not copy it.
- [Go blog: Context and structs](https://go.dev/blog/context-and-structs) — guidance on what belongs in a context value.
- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — closing a channel and the happens-before guarantee a receive gets from it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-pubsub-topic-handler-registration-loop-capture.md](25-pubsub-topic-handler-registration-loop-capture.md) | Next: [27-resource-pool-checkout-defer-release-starvation.md](27-resource-pool-checkout-defer-release-starvation.md)
