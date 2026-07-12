# Exercise 6: Config Reloader: Skip No-Op Reloads with a == Guard

Hot config reload is everywhere in backend services: a file watcher or a control
plane hands you a new config, and you apply it. But most "reloads" are identical to
what is already running, and applying them anyway churns connection pools, rewrites
DB rows, and invalidates caches for nothing. The fix is one line: return early when
`new == current`. This exercise builds that guard on a fully comparable `Config`
(embedded struct included) with a concurrency-safe swap.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
configreload/               independent module: example.com/configreload
  go.mod                    go 1.26
  configreload.go           Config (embedded PoolConfig); Reloader.Apply (== guard), atomic swap
  cmd/
    demo/
      main.go               runnable demo: identical no-op, changed field, embedded change
  configreload_test.go      no-op skip; exactly-once reload; embedded change; -race swap
```

- Files: `configreload.go`, `cmd/demo/main.go`, `configreload_test.go`.
- Implement: a comparable `Config` with an embedded `PoolConfig`; `Reloader.Apply(next) bool` that skips when `next == current`, swapping via `atomic.Pointer`.
- Test: identical config is a no-op (reload counter unchanged); a changed field reloads exactly once; an embedded-struct change is detected; a `-race` concurrent swap.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/05-struct-comparison-and-equality/06-config-reload-equality-guard/cmd/demo
cd go-solutions/07-structs-and-methods/05-struct-comparison-and-equality/06-config-reload-equality-guard
```

### Why keeping Config comparable is the design

The entire optimization rests on `next == current` being legal and meaningful, and
that is only true because `Config` is fully comparable. Its fields are a `string`, a
`bool`, and an embedded `PoolConfig` that is itself two `int`s — all comparable, so
`Config` is comparable, and `==` does a field-by-field comparison that *includes the
embedded struct's fields*. Embedding does not change comparability: an embedded
comparable struct is just more comparable fields, so a change to `PoolConfig.MaxConns`
alone makes the two `Config` values unequal and triggers a reload. `Apply` needs no
`Equal` method, no `reflect`, no per-field checklist — `==` is exact, fast, and
compile-checked.

This is a *constraint*, not just a convenience. If someone adds a `Tags []string` or
`Extra map[string]string` field to `Config`, the `next == current` line stops
compiling, and the no-op guard breaks. The comment in the code says so explicitly, so
the constraint is visible at the point that depends on it. If such a field is truly
needed, the guard has to move to a custom `Equal` (comparing the slice with
`slices.Equal`) — a deliberate, more expensive strategy, chosen knowingly.

The swap is concurrency-safe via `atomic.Pointer[Config]`: `Current` loads the
pointer and dereferences a stable snapshot, `Apply` stores a new pointer, and a
spy counter (`atomic.Int64`) records how many real reloads happened so tests can
assert the no-op path was actually skipped. Storing a `*Config` atomically (rather
than mutating fields under a lock) means a reader never observes a half-updated
config.

Create `configreload.go`:

```go
package configreload

import "sync/atomic"

// PoolConfig is embedded in Config. Both fields are comparable.
type PoolConfig struct {
	MaxConns int
	IdleSec  int
}

// Config is a fully comparable service configuration. Because every field
// (including the embedded PoolConfig) is comparable, Config values can be
// compared with ==. Adding a slice/map field here would break the == guard in
// Apply at compile time.
type Config struct {
	DSN   string
	Debug bool
	PoolConfig
}

// Reloader holds the live config and applies replacements, skipping no-ops.
type Reloader struct {
	current atomic.Pointer[Config]
	reloads atomic.Int64
}

// New returns a Reloader seeded with initial.
func New(initial Config) *Reloader {
	r := &Reloader{}
	c := initial
	r.current.Store(&c)
	return r
}

// Apply installs next if it differs from the current config and reports whether a
// reload actually happened. An identical config is a no-op: no swap, no counter
// bump, and (in a real system) no pool churn or redundant DB write.
func (r *Reloader) Apply(next Config) (reloaded bool) {
	cur := r.current.Load()
	if *cur == next { // == compares every field, embedded PoolConfig included
		return false
	}
	c := next
	r.current.Store(&c)
	r.reloads.Add(1)
	return true
}

// Current returns a snapshot of the live config.
func (r *Reloader) Current() Config { return *r.current.Load() }

// Reloads reports how many real reloads have occurred (no-ops excluded).
func (r *Reloader) Reloads() int64 { return r.reloads.Load() }
```

### The runnable demo

The demo applies an identical config (a no-op), then changes the DSN, then changes
only an embedded-struct field, then re-applies the last one (another no-op), and
prints the reload counter.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configreload"
)

func main() {
	base := configreload.Config{
		DSN:        "postgres://a",
		Debug:      false,
		PoolConfig: configreload.PoolConfig{MaxConns: 10, IdleSec: 30},
	}
	r := configreload.New(base)

	fmt.Printf("apply identical: %v\n", r.Apply(base))

	changed := base
	changed.DSN = "postgres://b"
	fmt.Printf("apply changed DSN: %v\n", r.Apply(changed))

	embedded := changed
	embedded.MaxConns = 20
	fmt.Printf("apply changed embedded: %v\n", r.Apply(embedded))

	fmt.Printf("apply same embedded again: %v\n", r.Apply(embedded))
	fmt.Printf("reloads: %d\n", r.Reloads())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
apply identical: false
apply changed DSN: true
apply changed embedded: true
apply same embedded again: false
reloads: 2
```

### Tests

`TestNoOpReloadSkipped` applies the same config repeatedly and asserts the reload
counter stays at zero. `TestReloadRunsExactlyOnce` applies a changed config, then
the same changed config again, and asserts exactly one reload. `TestEmbeddedFieldChangeDetected`
changes only `PoolConfig.MaxConns` and asserts it triggers a reload — proving `==`
sees through the embedding. `TestConcurrentSwap` runs many goroutines applying
distinct configs plus readers, under `-race`, and asserts every distinct apply
reloaded.

Create `configreload_test.go`:

```go
package configreload

import (
	"fmt"
	"sync"
	"testing"
)

func base() Config {
	return Config{
		DSN:        "postgres://a",
		Debug:      false,
		PoolConfig: PoolConfig{MaxConns: 10, IdleSec: 30},
	}
}

func TestNoOpReloadSkipped(t *testing.T) {
	t.Parallel()

	r := New(base())
	for range 5 {
		if r.Apply(base()) {
			t.Fatal("applying an identical config must be a no-op")
		}
	}
	if got := r.Reloads(); got != 0 {
		t.Fatalf("Reloads = %d, want 0", got)
	}
}

func TestReloadRunsExactlyOnce(t *testing.T) {
	t.Parallel()

	r := New(base())
	changed := base()
	changed.DSN = "postgres://b"

	if !r.Apply(changed) {
		t.Fatal("first apply of a changed config should reload")
	}
	if r.Apply(changed) {
		t.Fatal("re-applying the same changed config should be a no-op")
	}
	if got := r.Reloads(); got != 1 {
		t.Fatalf("Reloads = %d, want 1", got)
	}
	if r.Current().DSN != "postgres://b" {
		t.Fatalf("Current DSN = %q, want postgres://b", r.Current().DSN)
	}
}

func TestEmbeddedFieldChangeDetected(t *testing.T) {
	t.Parallel()

	r := New(base())
	changed := base()
	changed.MaxConns = 20 // embedded PoolConfig field

	if !r.Apply(changed) {
		t.Fatal("a change to an embedded-struct field must trigger a reload")
	}
	if r.Current().MaxConns != 20 {
		t.Fatalf("Current MaxConns = %d, want 20", r.Current().MaxConns)
	}
}

func TestConcurrentSwap(t *testing.T) {
	t.Parallel()

	const n = 50
	r := New(base())
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := base()
			c.DSN = fmt.Sprintf("postgres://host-%d", i) // unique, always differs
			r.Apply(c)
			_ = r.Current()
		}()
	}
	wg.Wait()

	if got := r.Reloads(); got != n {
		t.Fatalf("Reloads = %d, want %d (each distinct config reloads)", got, n)
	}
}
```

## Review

The reloader is correct when identical configs are skipped and any differing field
reloads exactly once. The `==` guard is the whole artifact, and `TestEmbeddedFieldChangeDetected`
is the assertion that proves embedding does not create a blind spot — a naive `Equal`
that compared only `DSN` and `Debug` would silently ignore pool changes, and that
test would catch it. Keep `Config` comparable: the day a slice/map field is genuinely
required, the `*cur == next` line will stop compiling, which is your signal to move
to a custom `Equal`, not to reach for `reflect.DeepEqual` on a reload path. The
`atomic.Pointer` swap is what makes `TestConcurrentSwap` race-clean; run it with
`go test -race`.

## Resources

- [sync/atomic.Pointer](https://pkg.go.dev/sync/atomic#Pointer) — the typed atomic used for the lock-free config swap.
- [Go spec: Struct types](https://go.dev/ref/spec#Struct_types) — embedded fields and how they participate in `==`.
- [Go spec: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — field-by-field struct comparison.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-generic-comparable-set.md](07-generic-comparable-set.md)
