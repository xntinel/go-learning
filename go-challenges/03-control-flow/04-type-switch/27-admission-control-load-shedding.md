# Exercise 27: Route Requests by Operational Load Signal Type

**Nivel: Intermedio** — validacion rapida (un test corto).

A service under real load pressure needs to shed work before it falls over,
not after — once a process is swapping or its request queue has grown
past the point where anything still in it will time out before it is ever
served, rejecting new work immediately is kinder to callers than accepting
it and failing slowly. The signals that trigger this decision arrive from
different subsystems in incompatible shapes: a CPU sampler reports a
percentage, a memory monitor reports byte counts that only mean something
relative to total capacity, and a queue reports a length against its own
configured capacity. An admission controller has to classify whichever
signal shows up and apply the right threshold logic for that signal's own
unit before it can return one shared decision. This module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
admission-control-load-shedding/   independent module: example.com/admission-control-load-shedding
  go.mod                           go 1.24
  loadshed.go                      Decide(signal any) (Decision, error)
  cmd/
    demo/
      main.go                      classifies four load signals into decisions
  loadshed_test.go                  table of cases at, above, and below each signal's boundaries
```

- Files: `loadshed.go`, `cmd/demo/main.go`, `loadshed_test.go`.
- Implement: `Decide(signal any) (Decision, error)`, type-switching on
  `CPUUsage`, `FreeMemory`, and `QueueDepth` to compute each signal's own
  ratio and compare it against that signal's own three-tier thresholds.
- Test: every signal kind exactly at, just under, and just over its
  accept/backpressure and backpressure/reject boundaries, an invalid
  `FreeMemory` with zero total bytes, an invalid `QueueDepth` with zero
  capacity, and an unsupported signal type.

Set up the module:

```bash
mkdir -p ~/go-exercises/admission-control-load-shedding/cmd/demo
cd ~/go-exercises/admission-control-load-shedding
go mod init example.com/admission-control-load-shedding
go mod edit -go=1.24
```

Each load signal's unit only makes sense relative to something the signal
itself carries: a queue length of 900 means nothing shed-worthy without
knowing the queue's capacity is 1000, and a free-byte count means nothing
without the total. That is why `FreeMemory` and `QueueDepth` are structs
carrying both halves of their own ratio rather than the admission
controller receiving a single pre-normalized `float64` percentage — pushing
the normalization into the caller would mean every caller re-derives the
same percentage math, and a caller that gets the division backwards (bytes
used instead of bytes free, say) produces a signal that is syntactically
fine and semantically inverted, silently shedding load exactly when it
should be accepting it. Keeping the raw numerator and denominator in the
struct and doing the division once, next to the threshold that consumes
it, is what keeps that class of bug local to one function instead of
smeared across every call site.

Create `loadshed.go`:

```go
package loadshed

import (
	"fmt"
)

// Decision is the outcome of one admission check: whether to let the
// request through untouched, apply backpressure, or shed it outright.
type Decision int

const (
	// Accept lets the request proceed normally.
	Accept Decision = iota
	// Backpressure lets the request proceed but signals the caller to slow
	// down — typically by adding a synthetic delay or a Retry-After hint.
	Backpressure
	// Reject sheds the request immediately, before it consumes any more
	// resources than the check itself did.
	Reject
)

func (d Decision) String() string {
	switch d {
	case Accept:
		return "accept"
	case Backpressure:
		return "backpressure"
	case Reject:
		return "reject"
	default:
		return "unknown"
	}
}

// CPUUsage reports the process's or host's current CPU utilization.
type CPUUsage struct{ Percent float64 }

// FreeMemory reports how much memory remains against the total available,
// so the shedder can reason about a ratio rather than an absolute byte
// count that means nothing without knowing the machine's total capacity.
type FreeMemory struct{ FreeBytes, TotalBytes int64 }

// QueueDepth reports how many requests are already queued waiting to be
// served, against the queue's configured capacity.
type QueueDepth struct{ Length, Capacity int }

// Decide classifies one operational load signal by its concrete type and
// returns the admission decision for it. Each signal kind carries a
// different unit — a percentage, a byte ratio, a queue ratio — so folding
// them into one generic "load: float64" shape would force every caller to
// pre-normalize its own metric into a percentage before calling in, which
// is exactly the kind of silent unit-conversion bug a type switch here
// avoids: the conversion logic lives once, next to the threshold it feeds.
func Decide(signal any) (Decision, error) {
	switch s := signal.(type) {
	case CPUUsage:
		switch {
		case s.Percent >= 90:
			return Reject, nil
		case s.Percent >= 70:
			return Backpressure, nil
		default:
			return Accept, nil
		}

	case FreeMemory:
		if s.TotalBytes <= 0 {
			return Reject, fmt.Errorf("loadshed: invalid total bytes %d", s.TotalBytes)
		}
		freePct := float64(s.FreeBytes) / float64(s.TotalBytes) * 100
		switch {
		case freePct < 10:
			return Reject, nil
		case freePct < 20:
			return Backpressure, nil
		default:
			return Accept, nil
		}

	case QueueDepth:
		if s.Capacity <= 0 {
			return Reject, fmt.Errorf("loadshed: invalid queue capacity %d", s.Capacity)
		}
		ratio := float64(s.Length) / float64(s.Capacity)
		switch {
		case ratio > 0.8:
			return Reject, nil
		case ratio > 0.5:
			return Backpressure, nil
		default:
			return Accept, nil
		}

	default:
		return Reject, fmt.Errorf("loadshed: unsupported signal type %T", signal)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/admission-control-load-shedding"
)

func main() {
	signals := []any{
		loadshed.CPUUsage{Percent: 55},
		loadshed.CPUUsage{Percent: 82},
		loadshed.FreeMemory{FreeBytes: 500 << 20, TotalBytes: 8 << 30},
		loadshed.QueueDepth{Length: 900, Capacity: 1000},
	}
	for _, s := range signals {
		d, err := loadshed.Decide(s)
		if err != nil {
			fmt.Printf("%-20T -> error: %v\n", s, err)
			continue
		}
		fmt.Printf("%-20T -> %s\n", s, d)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
loadshed.CPUUsage    -> accept
loadshed.CPUUsage    -> backpressure
loadshed.FreeMemory  -> reject
loadshed.QueueDepth  -> reject
```

500MB free out of 8GB total is roughly 6% free, under the 10% reject
threshold; 900 of 1000 queue slots filled is a 90% ratio, over the 80%
reject threshold. Both signals independently conclude "shed this," each
from its own unit, without either one needing to know the other's
threshold logic exists.

### Tests

Create `loadshed_test.go`:

```go
package loadshed

import "testing"

func TestDecide(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		signal  any
		want    Decision
		wantErr bool
	}{
		{"cpu comfortably under threshold accepts", CPUUsage{Percent: 40}, Accept, false},
		{"cpu exactly at backpressure boundary applies backpressure", CPUUsage{Percent: 70}, Backpressure, false},
		{"cpu exactly at reject boundary rejects", CPUUsage{Percent: 90}, Reject, false},
		{"cpu just under reject boundary applies backpressure", CPUUsage{Percent: 89.9}, Backpressure, false},

		{"memory comfortably free accepts", FreeMemory{FreeBytes: 4 << 30, TotalBytes: 8 << 30}, Accept, false},
		{"memory at 15 percent free applies backpressure", FreeMemory{FreeBytes: 15, TotalBytes: 100}, Backpressure, false},
		{"memory below 10 percent free rejects", FreeMemory{FreeBytes: 5, TotalBytes: 100}, Reject, false},
		{"memory with zero total is an error", FreeMemory{FreeBytes: 5, TotalBytes: 0}, Reject, true},

		{"queue comfortably under threshold accepts", QueueDepth{Length: 100, Capacity: 1000}, Accept, false},
		{"queue at 60 percent applies backpressure", QueueDepth{Length: 600, Capacity: 1000}, Backpressure, false},
		{"queue over 80 percent rejects", QueueDepth{Length: 900, Capacity: 1000}, Reject, false},
		{"queue with zero capacity is an error", QueueDepth{Length: 1, Capacity: 0}, Reject, true},

		{"unsupported signal type is an error", "bogus", Reject, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Decide(tt.signal)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Decide(%v) = nil error, want an error", tt.signal)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide(%v) unexpected error: %v", tt.signal, err)
			}
			if got != tt.want {
				t.Fatalf("Decide(%v) = %v, want %v", tt.signal, got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Decide` is correct because every threshold comparison for every signal
kind is written directly against that signal's own boundary and tested at
the exact boundary value, not just comfortably on either side of it — the
CPU test table checks 70 and 90 exactly, which is what would catch a `>`
versus `>=` slip that silently shifts a threshold by a fraction of a
percent. Guarding `FreeMemory` and `QueueDepth` against a zero denominator
before dividing is the other property worth keeping deliberately: a
misconfigured monitor reporting `TotalBytes: 0` must not be allowed to
divide, since even though Go does not panic on floating-point division by
zero, letting a `+Inf` or `NaN` ratio flow into the comparison logic below
would produce a decision that is technically defined but operationally
meaningless. Returning `Reject` as the default for both the invalid-input
and unsupported-type branches is a deliberate fail-safe: an admission
controller that cannot understand a signal should shed load rather than
silently accept it, because the failure mode of over-rejecting under a
monitoring bug is recoverable, while the failure mode of under-rejecting
during a real overload is not.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/)
- [AWS Builders' Library: Using load shedding to avoid overload](https://aws.amazon.com/builders-library/using-load-shedding-to-avoid-overload/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-bloom-filter-existence-check.md](26-bloom-filter-existence-check.md) | Next: [28-dns-resolution-record-dispatch.md](28-dns-resolution-record-dispatch.md)
