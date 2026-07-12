# Exercise 6: 64-bit atomic alignment, safe on 386/ARM/32-bit MIPS

A metrics struct with a 64-bit atomic counter works fine on your amd64 laptop and
panics the first time it runs on a 32-bit edge device — because a 64-bit atomic
requires 8-byte alignment the runtime cannot guarantee for an arbitrary `int64`
field on a 32-bit platform. This module builds the struct the safe way, using the
`atomic.Int64` wrapper that is auto-aligned everywhere, and pins the alignment
invariant in a test.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test.

## What you'll build

```text
atomicmetrics/             independent module: example.com/atomicmetrics
  go.mod                   go 1.26
  metrics.go               type Metrics with atomic.Int64/Uint64 fields
  cmd/
    demo/
      main.go              concurrent increments, prints a snapshot
  metrics_test.go          Offsetof(atomic field) % 8 == 0; Alignof == 8; -race totals
```

- Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
- Implement: a `Metrics` struct carrying 64-bit atomic counters via `atomic.Int64`/`atomic.Uint64`, with `IncRequests`, `IncErrors`, `AddBytes`, and `Snapshot`.
- Test: assert every 64-bit atomic field's offset is a multiple of 8, that `unsafe.Alignof(atomic.Int64{}) == 8`, and exercise the counters concurrently under `-race`.
- Verify: `go test -count=1 -race ./...`

### The failure mode, and the fix

A 64-bit atomic operation requires its operand to be 8-byte aligned. On a 64-bit
platform every `int64` field is 8-aligned automatically, so the requirement is
invisible. On a 32-bit platform (`386`, 32-bit `arm`, 32-bit `mips`) the natural
word is 4 bytes and `int64`'s alignment drops to 4, so a bare `int64` field can
land on a 4-byte-but-not-8-byte boundary. Passing the address of such a field to
the legacy `atomic.AddInt64` panics at runtime:

```
// DANGER — illustrative, do not ship. On a 32-bit platform int64 aligns to 4,
// so counter can sit at offset 4, and the legacy atomic panics there.
type badMetrics struct {
	flag    uint32 // offset 0
	counter int64  // 64-bit: offset 8 (aligned); 32-bit: offset 4 (MISALIGNED)
}

func (m *badMetrics) inc() {
	atomic.AddInt64(&m.counter, 1) // panics on 386/arm: unaligned 64-bit atomic
}
```

The bug never reproduces on amd64 or arm64 CI; it only bites when the binary runs
on the 32-bit target in the field. Two fixes exist. The old one: place the 64-bit
field first (offset 0 is always 8-aligned) and never let it move — fragile,
because a later refactor can reorder it. The modern one, and the one this module
uses: the `atomic.Int64` / `atomic.Uint64` wrapper types (Go 1.19+). They embed
an internal alignment marker so the runtime guarantees each is 8-byte aligned on
*every* platform, regardless of where the field sits in the struct. You call
`m.Requests.Add(1)` and `m.Requests.Load()`; alignment is the type's problem, not
yours. As a bonus, `go vet` (and `GOARCH=386 go vet` in particular) flags misuse
of the legacy bare-`int64` pattern, while the wrapper types are always correct.

Create `metrics.go`:

```go
// Package atomicmetrics carries 64-bit atomic counters that are alignment-safe on
// 32-bit platforms by using the auto-aligned atomic.Int64/Uint64 wrapper types.
package atomicmetrics

import "sync/atomic"

// Metrics is a set of server counters updated concurrently from many goroutines.
// Using atomic.Int64/Uint64 (not bare int64 + atomic.AddInt64) guarantees 8-byte
// alignment on 32-bit targets, so these updates never panic there.
type Metrics struct {
	Requests atomic.Int64
	Errors   atomic.Uint64
	Bytes    atomic.Int64
	name     string
}

// New returns a Metrics labeled with name.
func New(name string) *Metrics { return &Metrics{name: name} }

// IncRequests atomically increments the request counter.
func (m *Metrics) IncRequests() { m.Requests.Add(1) }

// IncErrors atomically increments the error counter.
func (m *Metrics) IncErrors() { m.Errors.Add(1) }

// AddBytes atomically adds n to the byte counter.
func (m *Metrics) AddBytes(n int64) { m.Bytes.Add(n) }

// Snapshot atomically reads the current counters.
func (m *Metrics) Snapshot() (requests int64, errs uint64, bytes int64) {
	return m.Requests.Load(), m.Errors.Load(), m.Bytes.Load()
}

// Name reports the metrics label.
func (m *Metrics) Name() string { return m.name }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/atomicmetrics"
)

func main() {
	m := atomicmetrics.New("api")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				m.IncRequests()
				m.AddBytes(512)
			}
		}()
	}
	wg.Wait()
	m.IncErrors()

	reqs, errs, bytes := m.Snapshot()
	fmt.Printf("%s: requests=%d errors=%d bytes=%d\n", m.Name(), reqs, errs, bytes)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
api: requests=8000 errors=1 bytes=4096000
```

### Tests

The layout test asserts the invariant the 32-bit runtime demands — each 64-bit
atomic field's offset is a multiple of 8 — plus that the wrapper type's own
alignment is 8. These hold on 64-bit trivially and, thanks to the wrapper's
internal marker, on 32-bit too. The concurrency test drives the counters from
many goroutines under `-race`.

Create `metrics_test.go`:

```go
package atomicmetrics

import (
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestAtomicFieldsAre8ByteAligned(t *testing.T) {
	t.Parallel()

	var m Metrics
	for _, tc := range []struct {
		name   string
		offset uintptr
	}{
		{"Requests", unsafe.Offsetof(m.Requests)},
		{"Errors", unsafe.Offsetof(m.Errors)},
		{"Bytes", unsafe.Offsetof(m.Bytes)},
	} {
		if tc.offset%8 != 0 {
			t.Errorf("%s offset = %d, not a multiple of 8; a 64-bit atomic there panics on 32-bit", tc.name, tc.offset)
		}
	}
}

func TestWrapperTypesAlignTo8(t *testing.T) {
	t.Parallel()

	if a := unsafe.Alignof(atomic.Int64{}); a != 8 {
		t.Errorf("Alignof(atomic.Int64{}) = %d, want 8", a)
	}
	if a := unsafe.Alignof(atomic.Uint64{}); a != 8 {
		t.Errorf("Alignof(atomic.Uint64{}) = %d, want 8", a)
	}
}

func TestConcurrentCountersRace(t *testing.T) {
	t.Parallel()

	m := New("test")
	const goroutines, per = 16, 500
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				m.IncRequests()
				m.AddBytes(8)
			}
		}()
	}
	wg.Wait()

	reqs, _, bytes := m.Snapshot()
	if want := int64(goroutines * per); reqs != want {
		t.Errorf("requests = %d, want %d", reqs, want)
	}
	if want := int64(goroutines * per * 8); bytes != want {
		t.Errorf("bytes = %d, want %d", bytes, want)
	}
}
```

## Review

The struct is correct when every 64-bit atomic field is 8-byte aligned on every
target, which the `atomic.Int64`/`atomic.Uint64` wrapper types guarantee by
construction — that is the whole reason to prefer them over a bare `int64` plus
`atomic.AddInt64`. The mistake this exercise exists to prevent is shipping the
legacy bare-`int64` pattern to a 32-bit edge device, where a field that happens to
land on a 4-byte boundary panics the moment you touch it atomically. If you must
use a bare `int64`, put it first and pin its offset with a test; the wrapper types
make that discipline unnecessary.

## Resources

- [sync/atomic: Int64/Uint64 wrapper types](https://pkg.go.dev/sync/atomic#Int64) — auto-aligned, the recommended approach.
- [sync/atomic bug note: alignment on 32-bit](https://pkg.go.dev/sync/atomic#pkg-note-BUG) — the official statement that 64-bit atomics require 8-byte alignment and how the runtime guarantees it.
- [go vet](https://pkg.go.dev/cmd/vet) — flags misaligned 64-bit atomic usage, especially under `GOARCH=386`.

---

Back to [05-cache-line-false-sharing.md](05-cache-line-false-sharing.md) | Next: [07-compact-hot-path-record.md](07-compact-hot-path-record.md)
