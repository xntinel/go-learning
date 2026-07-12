# Exercise 9: ABA-Safe Versioned CAS: Peak In-Flight Gauge

Pointer CAS in Go is ABA-safe because the GC guarantees address identity; value
CAS is not, and this is the module where that stops being a footnote. You build
a metrics gauge tracking current and peak in-flight requests, then a resettable
window maximum whose reset can race with updaters — and you make the race safe
by packing a version counter into the CAS word, next to an unversioned control
implementation that demonstrably loses the reset.

## What you'll build

```text
peakgauge/                       independent module: example.com/peakgauge
  go.mod
  gauge.go                       Gauge: Inc, Dec, Current, Peak (CAS max loop)
  window.go                      WindowMax: versioned uint64 (version<<32 | value);
                                 Snapshot, TryRecord, Record, Reset, Max, Version
                                 UnversionedMax: the ABA-prone control
  gauge_test.go                  barrier-exact peak, storm invariants, the targeted
                                 ABA interleaving (versioned rejects, control loses), Example
  cmd/
    demo/
      main.go                    staged in-flight phases; window record/reset/record
```

- Files: `gauge.go`, `window.go`, `gauge_test.go`, `cmd/demo/main.go`.
- Implement: `Gauge` with `atomic.Int64` current plus a CAS-maximum peak; `WindowMax` packing a 32-bit version and 32-bit value into one `atomic.Uint64`; `UnversionedMax` as the deliberately ABA-vulnerable control.
- Test: a concurrent Inc/Dec storm (peak bounded, current returns to zero); a two-phase barrier test where the peak is exact; the targeted interleaving that drives `Reset` between a stale updater's `Load` and its CAS.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/11-lock-free-data-structures/09-versioned-peak-gauge/cmd/demo
cd go-solutions/15-sync-primitives/11-lock-free-data-structures/09-versioned-peak-gauge
```

### The CAS-maximum loop

A high-water mark is the simplest CAS loop after a counter: load the peak, and
if your observation is higher, try to CAS it in; a failed CAS means someone
raised (or matched) the peak concurrently, so re-load and re-check — the retry
may discover there is nothing left to do, which is the early-return in the loop.
`Inc` returns the post-increment value from `Add`, so each goroutine feeds the
exact concurrency level *it* created into the maximum:

```go
cur := g.current.Add(1)
for {
	p := g.peak.Load()
	if cur <= p || g.peak.CompareAndSwap(p, cur) {
		return
	}
}
```

This peak is exact, not best-effort: every `Inc` that raised concurrency to k
attempts to publish k, and the CAS-maximum keeps the largest. What is *not*
guaranteed is that `Current` and `Peak` read together form a consistent pair —
they are two words, read at two instants. Fine for a dashboard, and module-level
honesty demands saying so.

### Where ABA actually bites: the resettable window

Operations teams want the peak *per scrape window*: read the max, reset it,
repeat every 15 seconds. Now `Reset` races with `Record`, and a plain value CAS
has a real ABA hole. Consider an unversioned max word:

1. Updater A loads the word: 0.
2. Updater B records 10 — the word is 10.
3. The scraper resets — the word is 0 again. The new window has begun; sample
   10 belongs to the old one.
4. A, resumed, CASes 0 to its pre-reset sample. The word matches — A cannot
   tell this 0 from the 0 it loaded — so the CAS *succeeds*, publishing an
   old-window observation into the new window. The reset is silently undone.

No memory was corrupted; every individual operation was atomic. The *history*
was lost, because CAS compares identity of bits, not provenance. The fix packs a
32-bit version into the high half of the word: `Reset` bumps the version, so
step 4's CAS compares `(ver 0, val 0)` against `(ver 1, val 0)` and fails.
The stale updater's retry then loads the new version and — this is the
semantic part — *drops its sample*, because a sample taken in the old window
must not count against the new one. `TryRecord` returning false on a version
change is what makes that decision possible; a blind retry loop would
re-publish the stale sample and reintroduce the bug at the semantic level.

32 bits of version means a reset racing an updater is misjudged only if
exactly 2^32 resets happen between the updater's load and its CAS — not a
realistic hazard for a scrape-window gauge. (The same technique at higher
stakes appends a generation tag to pointers in languages without GC.)

Create `gauge.go`:

```go
package peakgauge

import "sync/atomic"

// Gauge tracks current and peak in-flight requests. Peak is exact:
// every increment publishes its observed level through a CAS-maximum.
type Gauge struct {
	current atomic.Int64
	peak    atomic.Int64
}

// Inc marks a request in flight and folds the new level into the peak.
func (g *Gauge) Inc() {
	cur := g.current.Add(1)
	for {
		p := g.peak.Load()
		if cur <= p || g.peak.CompareAndSwap(p, cur) {
			return
		}
	}
}

// Dec marks a request finished.
func (g *Gauge) Dec() {
	g.current.Add(-1)
}

// Current reports the requests in flight right now.
func (g *Gauge) Current() int64 {
	return g.current.Load()
}

// Peak reports the highest concurrency ever observed.
func (g *Gauge) Peak() int64 {
	return g.peak.Load()
}
```

Create `window.go`:

```go
package peakgauge

import "sync/atomic"

// WindowMax is a resettable maximum safe against the reset/update ABA
// race: a 32-bit version in the high half of the word makes every
// Reset change the bits, so a CAS built on a pre-reset snapshot fails.
type WindowMax struct {
	state atomic.Uint64 // version<<32 | value
}

func pack(ver, val uint32) uint64 {
	return uint64(ver)<<32 | uint64(val)
}

func unpack(s uint64) (ver, val uint32) {
	return uint32(s >> 32), uint32(s)
}

// Snapshot returns the packed word for a later TryRecord.
func (w *WindowMax) Snapshot() uint64 {
	return w.state.Load()
}

// TryRecord attempts to fold sample into the window the snapshot was
// taken in. It returns false if the state moved — including a Reset —
// in which case the caller decides whether the sample still belongs.
func (w *WindowMax) TryRecord(snapshot uint64, sample uint32) bool {
	ver, val := unpack(snapshot)
	if sample <= val {
		return true // the snapshot already dominates the sample
	}
	return w.state.CompareAndSwap(snapshot, pack(ver, sample))
}

// Record folds sample into the current window. If a Reset intervenes
// mid-loop the sample is dropped: it was observed in the old window.
func (w *WindowMax) Record(sample uint32) {
	for {
		snap := w.state.Load()
		if w.TryRecord(snap, sample) {
			return
		}
		ver, _ := unpack(snap)
		if newVer, _ := unpack(w.state.Load()); newVer != ver {
			return // window rolled over; stale sample must not count
		}
	}
}

// Reset starts a new window: version+1, value 0.
func (w *WindowMax) Reset() {
	for {
		old := w.state.Load()
		ver, _ := unpack(old)
		if w.state.CompareAndSwap(old, pack(ver+1, 0)) {
			return
		}
	}
}

// Max reports the maximum recorded in the current window.
func (w *WindowMax) Max() uint32 {
	_, val := unpack(w.state.Load())
	return val
}

// Version reports the current window number (diagnostics and tests).
func (w *WindowMax) Version() uint32 {
	ver, _ := unpack(w.state.Load())
	return ver
}

// UnversionedMax is the ABA-prone control: same API, no version. Kept
// so the test suite can demonstrate the lost-reset failure mode.
type UnversionedMax struct {
	state atomic.Uint64
}

func (u *UnversionedMax) Snapshot() uint64 {
	return u.state.Load()
}

func (u *UnversionedMax) TryRecord(snapshot uint64, sample uint32) bool {
	if uint64(sample) <= snapshot {
		return true
	}
	return u.state.CompareAndSwap(snapshot, uint64(sample))
}

func (u *UnversionedMax) Reset() {
	u.state.Store(0)
}

func (u *UnversionedMax) Max() uint32 {
	return uint32(u.state.Load())
}
```

### Tests

The gauge tests come in two strengths. The barrier test is exact: 20 goroutines
`Inc` and rendezvous on a `WaitGroup`, so all 20 are provably in flight at once
— `Current` and `Peak` must both read exactly 20, and after the release phase
`Current` is 0 while `Peak` stays 20. The storm test is an invariant test:
random hold times give an unpredictable peak, so it asserts bounds (at least 1,
at most the goroutine count) and the return to zero.

The ABA test is the heart of the module, and it needs no goroutines: the race
is *simulated deterministically* by doing what the interleaving would do —
snapshot, then reset, then the stale `TryRecord`. The versioned word must
reject the stale CAS; the unversioned control must (wrongly, and provably)
accept it. Pinning the failure of the control implementation is what makes the
mitigation legible: delete the version field and a test *fails*, rather than a
production window silently over-reporting once a fortnight.

Create `gauge_test.go`:

```go
package peakgauge

import (
	"fmt"
	"sync"
	"testing"
)

func TestGaugeBarrierExactPeak(t *testing.T) {
	t.Parallel()

	const n = 20
	var g Gauge

	var inFlight, release, done sync.WaitGroup
	inFlight.Add(n)
	release.Add(1)
	for range n {
		done.Add(1)
		go func() {
			defer done.Done()
			g.Inc()
			inFlight.Done()
			release.Wait()
			g.Dec()
		}()
	}

	inFlight.Wait() // all n are provably in flight
	if got := g.Current(); got != n {
		t.Fatalf("Current at barrier = %d, want %d", got, n)
	}
	if got := g.Peak(); got != n {
		t.Fatalf("Peak at barrier = %d, want %d", got, n)
	}

	release.Done()
	done.Wait()
	if got := g.Current(); got != 0 {
		t.Fatalf("Current after drain = %d, want 0", got)
	}
	if got := g.Peak(); got != n {
		t.Fatalf("Peak after drain = %d, want %d (peak is sticky)", got, n)
	}
}

func TestGaugeStormInvariants(t *testing.T) {
	t.Parallel()

	const n = 32
	var g Gauge
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				g.Inc()
				g.Dec()
			}
		}()
	}
	wg.Wait()

	if got := g.Current(); got != 0 {
		t.Fatalf("Current after storm = %d, want 0", got)
	}
	if p := g.Peak(); p < 1 || p > n {
		t.Fatalf("Peak = %d, want in [1,%d]", p, n)
	}
}

func TestWindowMaxVersionedRejectsStaleCAS(t *testing.T) {
	t.Parallel()

	var w WindowMax
	w.Record(10)

	// A slow updater snapshots the window...
	stale := w.Snapshot()

	// ...the scraper resets it (new window, version bumped)...
	w.Reset()

	// ...and the updater tries to publish a pre-reset sample.
	if w.TryRecord(stale, 25) {
		t.Fatal("TryRecord succeeded against a reset window (ABA not prevented)")
	}
	if got := w.Max(); got != 0 {
		t.Fatalf("Max after rejected stale record = %d, want 0", got)
	}
	if got := w.Version(); got != 1 {
		t.Fatalf("Version = %d, want 1", got)
	}

	// Record drops a stale sample when the window rolled mid-loop:
	// a fresh sample in the new window still lands.
	w.Record(7)
	if got := w.Max(); got != 7 {
		t.Fatalf("Max after fresh record = %d, want 7", got)
	}
}

func TestUnversionedControlLosesReset(t *testing.T) {
	t.Parallel()

	var u UnversionedMax

	// Same interleaving as the versioned test:
	stale := u.Snapshot() // 0
	u.TryRecord(u.Snapshot(), 10)
	u.Reset() // word is 0 again: indistinguishable from the snapshot

	if !u.TryRecord(stale, 25) {
		t.Fatal("expected the ABA CAS to succeed on the unversioned control")
	}
	if got := u.Max(); got != 25 {
		t.Fatalf("Max = %d, want 25 (the lost-reset bug this control exists to show)", got)
	}
}

func TestWindowMaxConcurrentRecords(t *testing.T) {
	t.Parallel()

	var w WindowMax
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Record(uint32(i + 1))
		}()
	}
	wg.Wait()

	if got := w.Max(); got != 64 {
		t.Fatalf("Max after concurrent records = %d, want 64", got)
	}
}

func ExampleWindowMax() {
	var w WindowMax
	w.Record(3)
	w.Record(9)
	w.Record(4)
	fmt.Println(w.Max())
	w.Reset()
	fmt.Println(w.Max())
	// Output:
	// 9
	// 0
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/peakgauge"
)

func main() {
	var g peakgauge.Gauge
	for range 5 {
		g.Inc()
	}
	fmt.Printf("in flight: current=%d peak=%d\n", g.Current(), g.Peak())
	for range 5 {
		g.Dec()
	}
	fmt.Printf("drained:   current=%d peak=%d\n", g.Current(), g.Peak())

	var w peakgauge.WindowMax
	w.Record(3)
	w.Record(9)
	w.Record(4)
	fmt.Printf("window 0:  max=%d\n", w.Max())
	w.Reset()
	w.Record(2)
	fmt.Printf("window 1:  max=%d version=%d\n", w.Max(), w.Version())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in flight: current=5 peak=5
drained:   current=0 peak=5
window 0:  max=9
window 1:  max=2 version=1
```

## Review

Judge the module by its two exactness claims and one rejection claim. The peak
is exact because the CAS-maximum folds every increment's own observation in —
the barrier test would catch a `Store`-based "maximum" that lets a lower value
overwrite a higher one. The versioned window rejects the stale CAS because
`Reset` changes bits even when the value returns to zero — and the unversioned
control test is deliberately kept green to show the exact bug you are paying 32
bits to avoid. Watch the semantic detail in `Record`'s retry: on CAS failure it
distinguishes "another updater raised the max" (retry) from "the window rolled
over" (drop the sample); collapsing those two cases into a blind retry
reintroduces the lost-reset at the semantic level even though every CAS is
version-checked. Finally, remember `Current`/`Peak` are two independent words —
consistent enough for dashboards, not a transactional pair.

## Resources

- [ABA problem](https://en.wikipedia.org/wiki/ABA_problem) — the failure mode, and tagged/versioned words as the classic mitigation.
- [sync/atomic: Uint64.CompareAndSwap](https://pkg.go.dev/sync/atomic#Uint64.CompareAndSwap) — the single-word CAS the packed version rides on.
- [The Go Memory Model](https://go.dev/ref/mem) — why the barrier test's WaitGroup rendezvous makes "all n in flight" a sound claim.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-spsc-ring-buffer-telemetry.md](08-spsc-ring-buffer-telemetry.md) | Next: [../12-sync-oncevalue-oncefunc/00-concepts.md](../12-sync-oncevalue-oncefunc/00-concepts.md)
