# Exercise 19: Event Enrichment Pipeline: Shared Context Pointer Captured Then Mutated

**Nivel: Intermedio** — validacion rapida (un test corto).

An event pipeline builds one handler per event, each meant to stamp the event
with the trace ID in effect when it was registered. A later pipeline stage
reuses the SAME `*EnrichmentContext` instance for the next batch and mutates
its `TraceID` — a common shortcut in enrichment pipeline code. Because the
earlier handlers hold a pointer to the shared context, they retroactively
report whatever the context says NOW, not what it said when they were
registered.

## What you'll build

```text
eventenrich/                 independent module: example.com/eventenrich
  go.mod                     go 1.24
  eventenrich.go               EnrichmentContext, Handler, BuildHandlers, BuildHandlersBuggy
  eventenrich_test.go          table test: snapshot vs. leaked mutation
```

- Files: `eventenrich.go`, `eventenrich_test.go`.
- Implement: `BuildHandlers(ctx, events) []Handler` snapshotting `ctx.TraceID` per event at registration time; `BuildHandlersBuggy` closing over the `*EnrichmentContext` pointer directly.
- Test: one table test that registers handlers, mutates `ctx.TraceID` afterward (simulating the next batch's setup), then calls a handler.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The event variable is fine; the shared context pointer is not

`BuildHandlersBuggy` closes over both `event` and `ctx`. `event` is a
well-behaved per-iteration range variable on a `go 1.24` module — each
handler correctly reports its own event name. The bug is entirely in `ctx`:
a later pipeline stage mutates it AFTER these handlers are registered, when
it moves on to enriching the next batch. Since every handler holds that same
pointer, they all start reporting the new `TraceID`, including the ones
registered for the OLD batch. `BuildHandlers` fixes it by reading
`ctx.TraceID` into a local `traceID` at registration time — the handler
closes over that snapshot.

Create `eventenrich.go`:

```go
package eventenrich

// EnrichmentContext holds fields a pipeline stamps onto every event while a
// batch is being built, such as the trace ID currently in effect.
type EnrichmentContext struct {
	TraceID string
}

// Handler enriches and describes one event.
type Handler func() string

// BuildHandlersBuggy registers one handler per key, but every handler closes
// over a POINTER to the SAME shared EnrichmentContext. A later stage of the
// pipeline reuses that same context instance and mutates its TraceID for the
// next batch of events -- since every earlier handler still holds that
// pointer, they retroactively report the NEW trace ID instead of the one in
// effect when they were registered.
func BuildHandlersBuggy(ctx *EnrichmentContext, events []string) []Handler {
	handlers := make([]Handler, len(events))
	for i, event := range events {
		handlers[i] = func() string {
			return event + ":" + ctx.TraceID // BUG: reads the live, mutable context
		}
	}
	return handlers
}

// BuildHandlers registers one handler per event, each snapshotting the
// TraceID AT REGISTRATION TIME, so a later mutation to the shared context
// cannot change what an already-registered handler reports.
func BuildHandlers(ctx *EnrichmentContext, events []string) []Handler {
	handlers := make([]Handler, len(events))
	for i, event := range events {
		traceID := ctx.TraceID // snapshot taken now, not read later
		handlers[i] = func() string {
			return event + ":" + traceID
		}
	}
	return handlers
}
```

### Test

One table test registers handlers for `login` and `logout` against
`TraceID: "trace-1"`, mutates the context to `"trace-2"` (simulating the next
batch's setup reusing the same context instance), then calls the first
handler.

Create `eventenrich_test.go`:

```go
package eventenrich

import "testing"

func TestBuildHandlers(t *testing.T) {
	tests := []struct {
		name  string
		build func(*EnrichmentContext, []string) []Handler
		want  string // handlers[0]() after ctx.TraceID is mutated to trace-2
	}{
		{
			name:  "snapshot at registration keeps the original trace ID",
			build: BuildHandlers,
			want:  "login:trace-1",
		},
		{
			name:  "live context pointer leaks the later trace ID mutation",
			build: BuildHandlersBuggy,
			want:  "login:trace-2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &EnrichmentContext{TraceID: "trace-1"}
			handlers := tt.build(ctx, []string{"login", "logout"})

			// A later pipeline stage reuses the same context instance for
			// the next batch of events.
			ctx.TraceID = "trace-2"

			if got := handlers[0](); got != tt.want {
				t.Fatalf("handlers[0]() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

This is the loop-capture family applied to event enrichment: the range
variable is not the problem (`event` binds correctly on Go 1.22+), the
shared, mutable `*EnrichmentContext` is. Reusing one context instance across
batches is a realistic shortcut — it is also exactly the shape that makes
earlier handlers drift when a later stage bumps a shared field. `BuildHandlers`
shows the fix costs one extra local variable per registration: read the
field now, close over that value, and the handler stops caring what happens
to the context afterward.

## Resources

- [Go spec: Pointer types](https://go.dev/ref/spec#Pointer_types) — why closing over `*EnrichmentContext` captures the mutation stream, not a value.
- [Go blog: Closures](https://go.dev/tour/moretypes/25) — closures capturing variables, not values.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-distributed-transaction-connection-pool-lifo-close.md](18-distributed-transaction-connection-pool-lifo-close.md) | Next: [20-request-handler-context-cancel-goroutine-escape.md](20-request-handler-context-cancel-goroutine-escape.md)
