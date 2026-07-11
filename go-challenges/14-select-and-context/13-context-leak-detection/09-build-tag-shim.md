# Exercise 9: Zero-Overhead Production Shim Behind A Build Tag

Instrumentation that allocates a record, takes a lock, and touches a map on every
context creation is fine in CI and unacceptable on a hot request path. The
standard resolution is two files with an *identical* exported API behind opposite
build constraints: one tracks, one is a pass-through no-op. Callers write a single
code path; CI compiles with `-tags leakdetect` and pays for tracking, while the
production binary omits the tag and pays nothing.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
leakshim/                          module example.com/leakshim
  go.mod
  leakdetect_instrumented.go       //go:build leakdetect   -> the tracking Detector
  leakdetect_shim.go               //go:build !leakdetect  -> the zero-overhead no-op
  leakdetect_instrumented_test.go  //go:build leakdetect   -> asserts tracking
  leakdetect_shim_test.go          //go:build !leakdetect  -> asserts no-op
  cmd/
    demo/
      main.go                      one code path; output depends on the tag
```

Files: `leakdetect_instrumented.go`, `leakdetect_shim.go`, the two tagged tests, `cmd/demo/main.go`.
Implement: identical `Detector` API — `New`, `WithCancel`, `ActiveContexts`, `TotalCreated` — in both files; the shim delegates straight to `context.WithCancel`.
Test: under the default build the shim's `ActiveContexts`/`TotalCreated` are always 0; under `-tags leakdetect` they track. Both configurations compile and `go vet` clean.
Verify: `go test -count=1 -race ./...` (default = shim) and `go test -race -tags leakdetect ./...` (instrumented).

Set up the module:

```bash
mkdir -p ~/go-exercises/leakshim/cmd/demo
cd ~/go-exercises/leakshim
go mod init example.com/leakshim
```

### One API, two implementations, chosen at compile time

A `//go:build` constraint on the first line of a file decides whether that file is
part of the build. `leakdetect_instrumented.go` carries `//go:build leakdetect`, so
it compiles only when you pass `-tags leakdetect`. `leakdetect_shim.go` carries
`//go:build !leakdetect` — the negation — so it compiles in *every other* build,
which is the default and therefore what production ships. Because exactly one of
the two is ever in a build, they can and must declare the *same* exported symbols:
`type Detector`, `func New`, and the methods `WithCancel`, `ActiveContexts`,
`TotalCreated`. The compiler sees one `Detector`, never a redefinition.

That identical surface is the entire trick. Call sites import `leakshim`, write
`d := leakshim.New(...); ctx, cancel := d.WithCancel(parent); defer cancel()`, and
never branch on whether instrumentation is present. In CI the build has the tag and
`ActiveContexts` returns a real count; in production the build lacks the tag and the
same call compiles down to a plain `context.WithCancel` with no map, no lock, no
allocation beyond what the standard library already does. The shim's methods return
constants, so the optimizer erases the tracking entirely. The two tagged test files
encode the contract for each configuration: the shim is a no-op, the instrumented
build tracks — and each test only compiles under its own tag.

Create the shim — the default, zero-overhead implementation:

```go
// leakdetect_shim.go
//go:build !leakdetect

package leakshim

import (
	"context"
	"time"
)

// Detector is the production no-op. It carries no state.
type Detector struct{}

// New returns a no-op Detector. The grace period is accepted for API parity and
// ignored.
func New(_ time.Duration) *Detector { return &Detector{} }

// WithCancel delegates straight to context.WithCancel: no tracking, no allocation
// beyond the standard library's own.
func (d *Detector) WithCancel(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}

// ActiveContexts is always 0 in the production build.
func (d *Detector) ActiveContexts() int { return 0 }

// TotalCreated is always 0 in the production build.
func (d *Detector) TotalCreated() int64 { return 0 }
```

Create the instrumented implementation — same API, real tracking:

```go
// leakdetect_instrumented.go
//go:build leakdetect

package leakshim

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// record carries a non-zero-size field on purpose: a zero-size struct{} would
// make every new(record) return the same runtime.zerobase address, so distinct
// contexts would collapse to one map key. createdAt guarantees distinct pointers.
type record struct {
	createdAt time.Time
}

// Detector tracks outstanding contexts. Compiled only under -tags leakdetect.
type Detector struct {
	mu          sync.Mutex
	active      map[*record]struct{}
	created     atomic.Int64
	gracePeriod time.Duration
}

// New returns a tracking Detector.
func New(gracePeriod time.Duration) *Detector {
	return &Detector{active: make(map[*record]struct{}), gracePeriod: gracePeriod}
}

// WithCancel wraps context.WithCancel and tracks the context until it is Done.
func (d *Detector) WithCancel(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	r := &record{createdAt: time.Now()}
	d.mu.Lock()
	d.active[r] = struct{}{}
	d.mu.Unlock()
	d.created.Add(1)
	context.AfterFunc(ctx, func() {
		d.mu.Lock()
		delete(d.active, r)
		d.mu.Unlock()
	})
	return ctx, func() {
		cancel()
		d.mu.Lock()
		delete(d.active, r)
		d.mu.Unlock()
	}
}

// ActiveContexts returns the number of outstanding contexts.
func (d *Detector) ActiveContexts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.active)
}

// TotalCreated returns the number of contexts ever created.
func (d *Detector) TotalCreated() int64 { return d.created.Load() }
```

### The runnable demo

The demo is the same one code path in both worlds. Under the default build it
prints zeros (the shim); under `-tags leakdetect` it prints real counts.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/leakshim"
)

func main() {
	d := leakshim.New(10 * time.Millisecond)
	_, cancel := d.WithCancel(context.Background())
	defer cancel()

	fmt.Println("created:", d.TotalCreated())
	fmt.Println("active:", d.ActiveContexts())
}
```

Run it the production way and the instrumented way:

```bash
go run ./cmd/demo
go run -tags leakdetect ./cmd/demo
```

Expected output of `go run ./cmd/demo` (the shim: zero overhead, zero counts):

```
created: 0
active: 0
```

Expected output of `go run -tags leakdetect ./cmd/demo` (tracking on):

```
created: 1
active: 1
```

### Tests

Each test compiles only under its own tag, so the default `go test` runs the shim
test and `go test -tags leakdetect` runs the instrumented one.

Create `leakdetect_shim_test.go`:

```go
// leakdetect_shim_test.go
//go:build !leakdetect

package leakshim

import (
	"context"
	"testing"
	"time"
)

func TestShimIsNoOp(t *testing.T) {
	t.Parallel()

	d := New(time.Millisecond)
	_, cancel := d.WithCancel(context.Background())
	defer cancel()

	if got := d.ActiveContexts(); got != 0 {
		t.Errorf("shim ActiveContexts = %d, want 0", got)
	}
	if got := d.TotalCreated(); got != 0 {
		t.Errorf("shim TotalCreated = %d, want 0", got)
	}
}
```

Create `leakdetect_instrumented_test.go`:

```go
// leakdetect_instrumented_test.go
//go:build leakdetect

package leakshim

import (
	"context"
	"testing"
	"time"
)

func TestInstrumentedTracks(t *testing.T) {
	t.Parallel()

	d := New(time.Millisecond)
	_, cancel := d.WithCancel(context.Background())
	defer cancel()

	if got := d.ActiveContexts(); got != 1 {
		t.Errorf("instrumented ActiveContexts = %d, want 1", got)
	}
	if got := d.TotalCreated(); got != 1 {
		t.Errorf("instrumented TotalCreated = %d, want 1", got)
	}
}

// TestInstrumentedTracksConcurrent pins the map-key invariant: two outstanding
// contexts must be counted as two. A zero-size record would alias both to the
// runtime.zerobase address and collapse the count to 1, so this test is what
// catches that regression under -tags leakdetect.
func TestInstrumentedTracksConcurrent(t *testing.T) {
	t.Parallel()

	d := New(time.Millisecond)
	_, cancel1 := d.WithCancel(context.Background())
	defer cancel1()
	_, cancel2 := d.WithCancel(context.Background())
	defer cancel2()

	if got := d.ActiveContexts(); got != 2 {
		t.Errorf("instrumented ActiveContexts = %d, want 2 (record pointers must be distinct)", got)
	}
	if got := d.TotalCreated(); got != 2 {
		t.Errorf("instrumented TotalCreated = %d, want 2", got)
	}
}
```

## Review

The pattern is correct when both build configurations compile and pass their own
test: the default (shim) build reports zeros, and the `-tags leakdetect` build
tracks. The load-bearing detail is that the two files declare the *same* exported
API — if the signatures drift, the tag that selects the drifted file fails to
compile at the call site, which is exactly the failure you want (loud, at build
time). The negated constraint `//go:build !leakdetect` on the shim is what makes it
the default: production omits the tag and the optimizer erases the no-op methods,
so the diagnostics cost literally nothing on the hot path. Verify both worlds:
`go vet ./...` and `go vet -tags leakdetect ./...` must both be clean.

## Resources

- [Go build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — `//go:build` syntax, negation, and how tags select files.
- [go test -tags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — running the test suite under a build tag.
- [Build constraints in the spec-adjacent docs](https://pkg.go.dev/go/build#hdr-Build_Constraints) — the canonical description of constraint evaluation.

---

Back to [08-pprof-goroutine-triage.md](08-pprof-goroutine-triage.md) | Next: [10-detached-context-audit.md](10-detached-context-audit.md)
