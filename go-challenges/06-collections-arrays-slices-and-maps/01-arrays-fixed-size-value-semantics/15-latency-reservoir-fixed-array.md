# Exercise 15: A Fixed-Size Latency Reservoir with Race-Free Percentile Reads

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A latency-percentile dashboard needs two things that pull against each
other: it must keep sampling forever, from a request path that never stops,
and it must let a monitoring goroutine compute p50/p99 at any moment without
slowing that request path down or corrupting the samples mid-read.
Reservoir sampling over a fixed `[256]uint32` array is the classical answer
to the first half; this module's `Percentile` shows the second half —
because the reservoir field is an array, a single locked copy produces a
fully independent snapshot, and the actual sort runs lock-free over that
private copy while `Record` keeps mutating the live reservoir underneath it,
unobserved and unaffected.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
latencyreservoir/              module example.com/latencyreservoir
  go.mod                       go 1.24
  reservoir.go                 Reservoir{samples [256]uint32}; Record/Len/Percentile/P50/P99; Randomizer; ErrEmpty
  reservoir_test.go            scripted-rand replacement table, empty error, full-reservoir percentiles,
                                seeded determinism, concurrent Record+Percentile under -race,
                                ExampleReservoir_Percentile
```

- Files: `reservoir.go`, `reservoir_test.go`.
- Implement: `Reservoir` backed by `samples [Capacity]uint32` and a `Randomizer` interface (`Intn(n int) int`, satisfied by `*math/rand.Rand`) driving Algorithm R; `Record(latency uint32)`, `Len() int`, `Percentile(p int) (uint32, error)` (copies `samples` under lock, sorts the copy unlocked), `P50`/`P99` convenience wrappers.
- Test: a `fakeRand` with a scripted `Intn` sequence pins an exact in-capacity replacement and an exact discarded roll; `Percentile` on an empty reservoir returns `ErrEmpty`; `P50`/`P99` on a fully-filled reservoir match hand-computed expected values; the same seed fed twice produces byte-identical final reservoirs, two different seeds do not; concurrent `Record` and `Percentile` calls from many goroutines pass under `-race`; and `ExampleReservoir_Percentile` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/01-arrays-fixed-size-value-semantics/15-latency-reservoir-fixed-array
cd go-solutions/06-collections-arrays-slices-and-maps/01-arrays-fixed-size-value-semantics/15-latency-reservoir-fixed-array
go mod edit -go=1.24
```

### Why the array copy is what makes the percentile read safe

Algorithm R is what lets `Reservoir` keep a statistically uniform sample of
every latency it has ever seen using only `Capacity` memory: the first
`Capacity` samples always fill the array in order, and after that, sample
number `k` (0-indexed) survives with probability `Capacity/(k+1)`, replacing
a uniformly-random existing slot. The replacement decision needs a random
source, and that source is injected as a `Randomizer` interface rather than
called from `math/rand`'s global functions — the same fixed seed fed to
`rand.New(rand.NewSource(seed))` always produces the same sequence of
`Intn` calls, so the same sequence of `Record` calls always ends with
byte-identical reservoir contents. That determinism is what makes
`TestDeterministicWithSeededSource` possible at all.

The concurrency story is where the array type does its real work.
`Percentile` needs a sorted view of the current samples to compute a
percentile index into, but sorting in place would either have to hold the
`Reservoir`'s lock for the entire sort (blocking every concurrent `Record`
for however long the sort takes) or risk `sort.Slice` reordering elements
while `Record` is mid-write to the very same backing array. `samples` being
a `[Capacity]uint32` array field, not a slice, closes that gap for free:

```go
r.mu.Lock()
snapshot := r.samples // array value copy
n := ...
r.mu.Unlock()

view := snapshot[:n]
sort.Slice(view, ...) // sorts the caller's private copy, not r.samples
```

`snapshot := r.samples` deep-copies all 256 `uint32`s — a fixed, cheap, 1KB
copy — while the lock is held, and then the lock is released *before*
sorting. `view` is a slice header over `snapshot`'s own array, not
`r.samples`'s, so `sort.Slice` permutes only the private copy. From that
point on, `Record` can keep replacing slots in the live `Reservoir` for the
entire duration of the sort, and neither side observes a torn read or a data
race — the array copy is what makes "read a consistent picture without
blocking the writer" true by construction, not by careful lock discipline.
If `samples` were a `[]uint32` slice field instead, the same
`snapshot := r.samples` line would only copy the header, `view` would alias
`r.samples`'s backing array, and the sort would corrupt the live reservoir
out from under a concurrent `Record` — precisely the array-vs-slice trap
from earlier exercises in this lesson, here surfacing as a real data race
instead of a silent logic bug.

Create `reservoir.go`:

```go
// Package latencyreservoir implements reservoir sampling over a fixed-size
// array of request latencies, with race-free percentile reads while
// sampling continues concurrently.
package latencyreservoir

import (
	"errors"
	"sort"
	"sync"
)

// Capacity is the fixed number of latency samples the Reservoir retains.
// A request-latency histogram service (the kind that feeds a p50/p99
// dashboard) cannot afford to keep every sample it ever sees, so it keeps
// a bounded, statistically representative subset instead.
const Capacity = 256

// ErrEmpty means Percentile was called before any sample was recorded.
var ErrEmpty = errors.New("latencyreservoir: no samples recorded")

// Randomizer is the minimal random source Reservoir needs: a single
// Intn(n) method returning a uniform value in [0, n). It is satisfied by
// *math/rand.Rand. Injecting it, instead of calling math/rand's global
// functions directly, is what makes reservoir sampling reproducible: the
// same Randomizer sequence always produces the same final reservoir
// contents for the same sequence of Record calls.
type Randomizer interface {
	Intn(n int) int
}

// Reservoir implements Algorithm R reservoir sampling over a fixed-size
// [Capacity]uint32 array of latency samples (in the caller's chosen unit,
// e.g. microseconds). It keeps a statistically uniform random sample of
// every latency ever recorded, using O(Capacity) memory regardless of how
// many samples have been offered.
//
// Reservoir is safe for concurrent use by multiple goroutines: every method
// is protected by an internal mutex, and Percentile additionally copies the
// sample array under that lock before sorting it unlocked, so a concurrent
// Record never races with, or blocks for the duration of, a percentile
// computation.
type Reservoir struct {
	mu      sync.Mutex
	rng     Randomizer
	samples [Capacity]uint32
	seen    int // total samples ever offered, including ones later replaced
}

// New returns an empty Reservoir driven by rng for its replacement
// decisions once more than Capacity samples have been recorded.
func New(rng Randomizer) *Reservoir {
	return &Reservoir{rng: rng}
}

// Record offers one latency sample to the reservoir. The first Capacity
// samples always fill the array in order. After that, sample number k
// (0-indexed) survives with probability Capacity/(k+1): a uniformly chosen
// existing slot is evicted in its favor, or it is discarded if the chosen
// slot falls outside the reservoir. This is the classical Algorithm R and
// it guarantees every sample seen so far has equal probability of being in
// the final reservoir, with no need to know the total count in advance.
func (r *Reservoir) Record(latency uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.seen < Capacity {
		r.samples[r.seen] = latency
	} else {
		j := r.rng.Intn(r.seen + 1)
		if j < Capacity {
			r.samples[j] = latency
		}
	}
	r.seen++
}

// Len reports how many valid slots are currently filled: min(samples
// recorded, Capacity).
func (r *Reservoir) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.seen < Capacity {
		return r.seen
	}
	return Capacity
}

// Percentile returns the p-th percentile (0-100) latency currently held in
// the reservoir. It copies the live [Capacity]uint32 array by value while
// holding the lock -- a cheap, fixed-size copy -- then releases the lock
// and sorts the copy. Because samples is an array, not a slice, that copy
// is a true independent snapshot: sort.Slice below mutates only the
// caller's private view[], never the Reservoir's own backing storage, so a
// concurrent Record can keep replacing slots in the live reservoir for the
// entire duration of the sort without racing or blocking on it.
func (r *Reservoir) Percentile(p int) (uint32, error) {
	r.mu.Lock()
	snapshot := r.samples // array value copy, independent of r.samples from here on
	n := r.seen
	if n > Capacity {
		n = Capacity
	}
	r.mu.Unlock()

	if n == 0 {
		return 0, ErrEmpty
	}

	view := snapshot[:n]
	sort.Slice(view, func(i, j int) bool { return view[i] < view[j] })

	idx := p * n / 100
	if idx >= n {
		idx = n - 1
	}
	return view[idx], nil
}

// P50 returns the median latency currently held in the reservoir.
func (r *Reservoir) P50() (uint32, error) {
	return r.Percentile(50)
}

// P99 returns the 99th-percentile latency currently held in the reservoir.
func (r *Reservoir) P99() (uint32, error) {
	return r.Percentile(99)
}
```

### Using it

Construct one `Reservoir` per metric with `New`, feeding it a seeded
`*rand.Rand` in tests and a process-global source (still seeded once, not
reseeded per call) in production. Every request-handling goroutine calls
`Record` after it knows the latency of the request it just served; any
goroutine — a `/metrics` handler, a periodic logger — calls `P50`/`P99`
whenever it needs a read, with no coordination required beyond what
`Reservoir` already does internally.

The module has no `main.go`, because a latency reservoir is a library, not
a tool. Its executable demonstration is `ExampleReservoir_Percentile`: `go
test` runs it and compares its standard output against the `// Output:`
comment, so the usage shown below cannot drift away from the code. It feeds
a synthetic stream of 5000 latencies from a seeded source — 95% fast
(8-12ms), 5% a slow tail (100-499ms) — into a `Reservoir` driven by its own
separate seeded source, then prints the resulting p50 and p99. The
reservoir size caps at `Capacity` (256) even though 5000 samples were
offered, exactly as designed, and both percentiles are reproducible
byte-for-byte on every run because both the synthetic generator and the
`Reservoir`'s own replacement decisions come from fixed seeds.

### Tests

`TestRecordFillsUpToCapacity` confirms the first `Capacity` records always
fill sequentially with no random source consulted at all.
`TestRecordReplacesInsideCapacity` is the sharpest correctness test: a
`fakeRand` scripts the exact `Intn` results for two post-capacity records,
so the test can assert *exactly* which slot got replaced and that a roll
landing outside `[0, Capacity)` is correctly discarded, without depending on
any particular PRNG's actual output sequence. `TestPercentileEmpty` covers
the base case. `TestPercentileOverFullReservoir` records values `1..Capacity`
in order (no random rolls needed) and checks `P50`/`P99` against
hand-computed expected values from the same index formula the
implementation uses.

`TestDeterministicWithSeededSource` is the determinism proof the exercise is
named for: two runs seeded identically produce byte-identical
`[Capacity]uint32` arrays, and a different seed does not.
`TestConcurrentRecordAndPercentile` drives `Record` from eight goroutines
and `Percentile` from four more, all at once, and must pass under `-race` —
proving the locked-copy-then-unlocked-sort pattern in `Percentile` really is
race-free.

Create `reservoir_test.go`:

```go
package latencyreservoir

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
)

// fakeRand is a Randomizer whose Intn results are scripted in advance, so a
// test can pin the exact replacement decision Algorithm R makes at a given
// step instead of depending on any particular PRNG's output sequence.
type fakeRand struct {
	seq []int
	i   int
}

func (f *fakeRand) Intn(n int) int {
	v := f.seq[f.i]
	f.i++
	return v
}

func TestRecordFillsUpToCapacity(t *testing.T) {
	t.Parallel()

	r := New(&fakeRand{})
	for i := 0; i < Capacity; i++ {
		r.Record(uint32(i))
	}

	if got := r.Len(); got != Capacity {
		t.Fatalf("Len() = %d, want %d", got, Capacity)
	}
	// No fakeRand.Intn call was needed: the first Capacity records always
	// fill sequentially without consulting the random source.
}

func TestRecordReplacesInsideCapacity(t *testing.T) {
	t.Parallel()

	// Fill the reservoir with values 0..255, then offer two more samples.
	// The first replacement roll (5) is inside [0, Capacity) and must land
	// in slot 5. The second roll (300) is outside [0, Capacity) and must
	// be discarded, leaving slot 5's new value untouched.
	r := New(&fakeRand{seq: []int{5, 300}})
	for i := 0; i < Capacity; i++ {
		r.Record(uint32(i))
	}

	r.Record(9999) // seen=256, rng.Intn(257) -> 5: samples[5] = 9999
	r.Record(8888) // seen=257, rng.Intn(258) -> 300: discarded

	full := r.samples
	if full[5] != 9999 {
		t.Fatalf("samples[5] = %d, want 9999", full[5])
	}
	for i, v := range full {
		if i == 5 {
			continue
		}
		if v != uint32(i) {
			t.Fatalf("samples[%d] = %d, want %d (should be untouched)", i, v, i)
		}
	}
	if got := r.Len(); got != Capacity {
		t.Fatalf("Len() = %d, want %d", got, Capacity)
	}
}

func TestPercentileEmpty(t *testing.T) {
	t.Parallel()

	r := New(&fakeRand{})
	if _, err := r.Percentile(50); err != ErrEmpty {
		t.Fatalf("Percentile on empty reservoir = %v, want ErrEmpty", err)
	}
}

func TestPercentileOverFullReservoir(t *testing.T) {
	t.Parallel()

	// Record exactly Capacity samples with values 1..Capacity, in order.
	// No replacement rolls are needed, so the fake source never gets
	// consulted and the sorted view is exactly [1, 2, ..., Capacity].
	r := New(&fakeRand{})
	for i := 1; i <= Capacity; i++ {
		r.Record(uint32(i))
	}

	p50, err := r.P50()
	if err != nil {
		t.Fatalf("P50: %v", err)
	}
	if p50 != 129 { // idx = 50*256/100 = 128, sorted[128] = 129
		t.Fatalf("P50() = %d, want 129", p50)
	}

	p99, err := r.P99()
	if err != nil {
		t.Fatalf("P99: %v", err)
	}
	if p99 != 254 { // idx = 99*256/100 = 253, sorted[253] = 254
		t.Fatalf("P99() = %d, want 254", p99)
	}
}

func TestDeterministicWithSeededSource(t *testing.T) {
	t.Parallel()

	run := func(seed int64) [Capacity]uint32 {
		r := New(rand.New(rand.NewSource(seed)))
		for i := 0; i < 10_000; i++ {
			r.Record(uint32(i))
		}
		return r.samples
	}

	a := run(42)
	b := run(42)
	if a != b {
		t.Fatal("two runs with the same seed produced different reservoirs")
	}

	c := run(43)
	if a == c {
		t.Fatal("two runs with different seeds produced identical reservoirs (suspiciously deterministic)")
	}
}

// TestConcurrentRecordAndPercentile drives Record from many goroutines
// concurrently with Percentile reads. The Reservoir's own mutex already
// serializes access to samples and to rng, so this must be race-clean
// under -race; Percentile's array copy means a concurrent Record can keep
// running for the whole duration of a Percentile's sort without either one
// observing a torn read.
func TestConcurrentRecordAndPercentile(t *testing.T) {
	t.Parallel()

	r := New(rand.New(rand.NewSource(7)))
	var wg sync.WaitGroup

	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				r.Record(uint32(g*500 + i))
			}
		}(g)
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, err := r.P50(); err != nil && err != ErrEmpty {
					t.Errorf("P50: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	if got := r.Len(); got != Capacity {
		t.Fatalf("Len() = %d, want %d", got, Capacity)
	}
}

// ExampleReservoir_Percentile is the runnable demonstration of this module:
// go test executes it and compares its stdout against the Output comment
// below. It feeds a synthetic stream of 5000 latencies from a seeded
// source -- 95% fast (8-12ms), 5% a slow tail (100-499ms) -- into a
// Reservoir driven by its own separate seeded source, then prints the
// resulting p50 and p99.
func ExampleReservoir_Percentile() {
	gen := rand.New(rand.NewSource(1))
	r := New(rand.New(rand.NewSource(2)))

	const total = 5000
	for i := 0; i < total; i++ {
		var latency uint32
		if gen.Intn(100) < 95 {
			latency = uint32(8 + gen.Intn(5)) // 8-12ms, the common case
		} else {
			latency = uint32(100 + gen.Intn(400)) // 100-499ms, the tail
		}
		r.Record(latency)
	}

	p50, err := r.P50()
	if err != nil {
		panic(err)
	}
	p99, err := r.P99()
	if err != nil {
		panic(err)
	}

	fmt.Printf("recorded: %d, reservoir size: %d\n", total, r.Len())
	fmt.Printf("p50: %dms\n", p50)
	fmt.Printf("p99: %dms\n", p99)

	// Output:
	// recorded: 5000, reservoir size: 256
	// p50: 10ms
	// p99: 435ms
}
```

## Review

The reservoir is correct on the sampling side when
`TestRecordReplacesInsideCapacity` pins Algorithm R's exact behavior at the
boundary -- a scripted, in-range roll replaces the right slot, a scripted,
out-of-range roll is discarded -- and `TestDeterministicWithSeededSource`
proves that behavior is fully reproducible given a fixed seed, which is
what makes a reservoir usable in a test suite or a reproducible incident
replay at all. It is correct on the concurrency side when
`TestConcurrentRecordAndPercentile` passes under `-race`: eight writers and
four readers hammering the same `Reservoir` with no data race is the direct
payoff of `Percentile` copying the `[Capacity]uint32` array under a short
lock and sorting the private copy unlocked. The mistake this design avoids
is sorting `r.samples[:n]` in place while holding a shorter lock, or worse,
not holding a lock at all during the sort -- either one lets `sort.Slice`'s
in-flight swaps interleave with a concurrent `Record`'s writes to the same
backing array, which `-race` would catch immediately and a production
dashboard would otherwise surface as corrupted, nonsensical percentiles
under load. Run `go test -count=1 -race ./...`.

## Resources

- [Reservoir sampling (Wikipedia)](https://en.wikipedia.org/wiki/Reservoir_sampling) — Algorithm R, the classical algorithm `Record` implements.
- [math/rand package](https://pkg.go.dev/math/rand) — `rand.New(rand.NewSource(seed))`, the seeded source injected as this module's `Randomizer`.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the short critical section that makes the array copy in `Percentile` a consistent snapshot.
- [Go Specification: Assignability and struct copies](https://go.dev/ref/spec#Assignments) — why `snapshot := r.samples` is a full, independent array copy rather than an aliasing slice-header copy.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-cache-line-padded-shard-counters.md](14-cache-line-padded-shard-counters.md) | Next: [16-hotp-truncation-hmac-array.md](16-hotp-truncation-hmac-array.md)
