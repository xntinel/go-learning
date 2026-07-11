# Exercise 4: Tag goroutines with tenant and request-id so dumps are attributable

A goroutine dump under load tells you a thousand goroutines are stuck on
`chan receive` — but not *whose* requests they are. This exercise attaches pprof
labels at the request boundary so every goroutine a handler spawns carries its
tenant and request-id, turning an anonymous dump into an attributable one you can
grep by tenant.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
attribution/                 independent module: example.com/attribution
  go.mod
  attribution.go             Handle (pprof.Do labels), Spawn (WithLabels/SetGoroutineLabels),
                             LabelOf, LabelsOf, GoroutineDump
  cmd/demo/main.go           run labeled work, show a dump attributes the tenant
  attribution_test.go        label-in-scope, label-in-dump, no-leak-outside-Do, Spawn tests
```

- Files: `attribution.go`, `cmd/demo/main.go`, `attribution_test.go`.
- Implement: `Handle(ctx, tenant, requestID, fn)` running `fn` under `pprof.Do` with those labels; `Spawn` for a manually-started labeled goroutine; `LabelOf`/`LabelsOf` readers; `GoroutineDump` rendering the goroutine profile (debug=1) with its `# labels:` lines.
- Test: inside `Handle`, `LabelOf(ctx, "tenant")` returns the tenant; a labeled parked goroutine's tenant appears in the dump; labels do not leak outside `Do`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/attribution/cmd/demo
cd ~/go-exercises/attribution
go mod init example.com/attribution
```

### How labels attach and propagate

`pprof.Do(ctx, labels, fn)` sets the given labels on the *current* goroutine for the
duration of `fn`, and — the property that makes it useful — any goroutine started
*inside* `fn` inherits them. So if request middleware wraps the handler in
`pprof.Do(ctx, pprof.Labels("tenant", t, "request_id", id), handler)`, then a worker
the handler launches with `go ...` is tagged with that tenant and request-id
automatically. The goroutine profile records those labels, and rendering it with
`debug=1` prints a `# labels: {"request_id":"...", "tenant":"..."}` line above each
stack. Now a dump you take at 03:00 attributes each stuck goroutine to a tenant.

The boundaries are strict, and the tests pin them. Labels attach only inside the
labeled scope and to children spawned there; a goroutine that already existed, or
work outside `Do`, carries nothing. For a goroutine you start yourself outside a
`Do` block — a long-lived background worker, say — you build a labeled context with
`pprof.WithLabels(ctx, ...)` and call `pprof.SetGoroutineLabels(ctx)` as the first
line the goroutine runs; that is what `Spawn` does. You read a single label back with
`pprof.Label(ctx, key)` and enumerate them with `pprof.ForLabels`.

`GoroutineDump` renders `pprof.Lookup("goroutine").WriteTo(w, 1)` — the aggregated,
human-readable form that includes the label lines. (debug=2 gives full per-goroutine
stacks but is a raw panic-style dump; debug=1 is the one that carries labels.)

Create `attribution.go`:

```go
package attribution

import (
	"context"
	"runtime/pprof"
	"strings"
)

// Handle runs fn with goroutine labels {tenant, request_id} attached. Any
// goroutine spawned inside fn inherits the labels, so a dump attributes it.
func Handle(ctx context.Context, tenant, requestID string, fn func(context.Context)) {
	labels := pprof.Labels("tenant", tenant, "request_id", requestID)
	pprof.Do(ctx, labels, fn)
}

// Spawn starts a goroutine that carries a "worker" label, using WithLabels and
// SetGoroutineLabels because the goroutine is created outside a pprof.Do scope.
// fn receives the labeled context so it can read its own labels back.
func Spawn(ctx context.Context, worker string, fn func(context.Context)) {
	lctx := pprof.WithLabels(ctx, pprof.Labels("worker", worker))
	go func() {
		pprof.SetGoroutineLabels(lctx)
		fn(lctx)
	}()
}

// LabelOf returns the value of a single label attached to ctx.
func LabelOf(ctx context.Context, key string) (string, bool) {
	return pprof.Label(ctx, key)
}

// LabelsOf collects every label visible on ctx.
func LabelsOf(ctx context.Context) map[string]string {
	out := map[string]string{}
	pprof.ForLabels(ctx, func(k, v string) bool {
		out[k] = v
		return true
	})
	return out
}

// GoroutineDump renders the goroutine profile with debug=1, which includes a
// "# labels:" line for each labeled goroutine.
func GoroutineDump() string {
	var b strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
	return b.String()
}
```

### The runnable demo

The demo runs a labeled unit of work, reads its tenant back inside the scope, spawns
a child goroutine that parks (so a labeled goroutine exists in the dump), takes a
dump and shows the tenant appears in it, and finally confirms the label does not
leak outside the `Do` scope.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"example.com/attribution"
)

func main() {
	attribution.Handle(context.Background(), "acme", "req-7", func(ctx context.Context) {
		tenant, _ := attribution.LabelOf(ctx, "tenant")
		fmt.Println("in-scope tenant:", tenant)

		block := make(chan struct{})
		started := make(chan struct{})
		go func() {
			close(started)
			<-block
		}()
		<-started

		dump := attribution.GoroutineDump()
		fmt.Println("dump attributes tenant:", strings.Contains(dump, "acme"))
		close(block)
	})

	_, ok := attribution.LabelOf(context.Background(), "tenant")
	fmt.Println("label outside scope:", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in-scope tenant: acme
dump attributes tenant: true
label outside scope: false
```

### Tests

`TestLabelInScope` proves `pprof.Label` reads the tenant back inside `Handle`.
`TestLabelInDump` spawns a child goroutine inside the labeled scope, parks it, and
asserts the tenant value appears in the goroutine profile — the whole point of
labeling. `TestLabelsDoNotLeakOutsideDo` confirms a bare context carries no tenant.
`TestSpawnLabelsGoroutine` checks the manual `WithLabels`/`SetGoroutineLabels` path
tags a goroutine started outside `Do`.

Create `attribution_test.go`:

```go
package attribution

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestLabelInScope(t *testing.T) {
	Handle(context.Background(), "acme", "req-1", func(ctx context.Context) {
		if v, ok := LabelOf(ctx, "tenant"); !ok || v != "acme" {
			t.Errorf("tenant label = %q,%v; want acme,true", v, ok)
		}
		if v, ok := LabelOf(ctx, "request_id"); !ok || v != "req-1" {
			t.Errorf("request_id label = %q,%v; want req-1,true", v, ok)
		}
	})
}

func TestLabelInDump(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})

	Handle(context.Background(), "acme", "req-42", func(ctx context.Context) {
		go func() {
			close(started)
			<-block // parked, labeled, so it appears in the dump
		}()
		<-started

		dump := GoroutineDump()
		if !strings.Contains(dump, "acme") {
			t.Errorf("dump missing tenant label 'acme':\n%s", dump)
		}
		close(block)
	})
}

func TestLabelsDoNotLeakOutsideDo(t *testing.T) {
	if _, ok := LabelOf(context.Background(), "tenant"); ok {
		t.Error("tenant label leaked onto a bare context")
	}
}

func TestSpawnLabelsGoroutine(t *testing.T) {
	block := make(chan struct{})
	got := make(chan string, 1)

	Spawn(context.Background(), "indexer", func(ctx context.Context) {
		v, _ := LabelOf(ctx, "worker")
		got <- v
		<-block
	})

	if v := <-got; v != "indexer" {
		t.Fatalf("worker label = %q; want indexer", v)
	}
	if !strings.Contains(GoroutineDump(), "indexer") {
		t.Error("dump missing worker label 'indexer'")
	}
	close(block)
}

func ExampleHandle() {
	Handle(context.Background(), "acme", "r1", func(ctx context.Context) {
		v, _ := LabelOf(ctx, "tenant")
		fmt.Println(v)
	})
	// Output: acme
}
```

## Review

Attribution is correct when a label set at the request boundary follows the work:
`LabelOf` reads it back inside the scope, a child goroutine spawned in the scope
inherits it, and the dump prints it — while a context outside the scope carries
nothing. `TestLabelInDump` is the load-bearing test, because the payoff is not that
`Label` round-trips (that is bookkeeping) but that an *anonymous parked goroutine* in
a real dump names its tenant. The `Spawn` path exists for goroutines you start
outside `Do`: without `SetGoroutineLabels`, `WithLabels` alone would label the
context but not the goroutine's profile entry. The common trap this guards against is
expecting labels to attach retroactively — they never touch a goroutine that already
existed. Run `go test -race`; the label machinery is read concurrently by the
profiler while the workers run.

## Resources

- [`runtime/pprof.Do`](https://pkg.go.dev/runtime/pprof#Do) — sets labels for the calling goroutine and its children for the duration of a function.
- [`runtime/pprof.Labels` / `Label` / `ForLabels` / `WithLabels` / `SetGoroutineLabels`](https://pkg.go.dev/runtime/pprof#Labels) — building, reading, and manually attaching label sets.
- [Profiler labels in Go](https://rakyll.org/profiler-labels/) — how labels attach to goroutines and appear in profiles.

---

Prev: [03-goroutine-leak-guard-in-tests.md](03-goroutine-leak-guard-in-tests.md) | Back to [00-concepts.md](00-concepts.md) | Next: [05-block-profile-channel-contention.md](05-block-profile-channel-contention.md)
