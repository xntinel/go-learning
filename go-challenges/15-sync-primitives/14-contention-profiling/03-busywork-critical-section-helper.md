# Exercise 3: Model a realistic critical section with a shared busyWork helper

A contention profile only shows something if the lock is actually held long enough
for goroutines to queue behind it. Real handlers hold a lock across genuine work —
a serialization, a hash, a bounds check. In a benchmark you stand that work in with
a spin helper, but the helper has one non-obvious requirement: the compiler must
not be able to optimize it away. This module builds `busyWork` correctly, proves it
does measurable non-zero work with a benchmark, and shows exactly how the elision
trap makes a contention signal silently disappear.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
busywork/                     independent module: example.com/busywork
  go.mod                      go 1.26
  busy.go                     busyWork (non-elidable), a package sink, and guard
  cmd/
    demo/
      main.go                 runnable demo: print busyWork sums
  busy_test.go                arithmetic test, BenchmarkBusyWork with ReportMetric
```

- Files: `busy.go`, `cmd/demo/main.go`, `busy_test.go`.
- Implement: a `busyWork(iterations)` spin helper written so the compiler cannot elide it (accumulate, then return the result and have callers observe it), plus a `guard` used inside locked regions.
- Test: a table test pinning `busyWork`'s arithmetic; a `BenchmarkBusyWork` that records ns/op with `b.ReportMetric` so a reviewer can confirm the helper does real, non-zero work.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/busywork/cmd/demo
cd ~/go-exercises/busywork
go mod init example.com/busywork
```

### Why elision would silence the whole chapter

The point of `busyWork` is to occupy the lock. A store's `Increment` does
`data[key]++` — a handful of instructions — and if that were all it did under the
lock, goroutines would grab and release the lock so fast they would almost never
queue. The mutex profile would be empty and every "fix" in the chapter would be
measuring noise. `busyWork` stands in for the real work a production handler does
while holding a lock.

The catch is that the Go compiler is aggressive about dead code. If `busyWork`
accumulated into a local and then returned nothing, or returned a value the caller
discarded, the optimizer would prove the loop has no observable effect and delete
it entirely. The lock would then be held for zero time again, and the contention
signal — the entire reason the helper exists — would be an artifact that vanishes
the moment you turn on optimizations. This is a real and easy-to-miss profiling
bug: your benchmark looks fine, the profile is mysteriously empty, and the cause is
that the work you thought you were doing was compiled out.

The defense is to make the result observable. `busyWork` returns its accumulated
sum. Callers inside a locked region pass it to `guard`, which compares it against a
value it can never equal (`1<<63` for this accumulation) and, on that impossible
branch, writes it to a package-level `sink`. The compiler cannot prove the branch
is dead without evaluating the loop, and it cannot delete a write to a
package-level variable, so the work survives. The `guard` call itself costs a
single comparison — negligible next to the loop it protects. In tests you observe
the return value directly, which is an even stronger guarantee: a value a test
reads cannot be elided.

Create `busy.go`:

```go
package busywork

// busyWork simulates work performed while holding a lock, so that lock contention
// is observable in a mutex or block profile. It returns the accumulated sum: a
// discarded result would let the compiler delete the loop, and then the lock
// would be held for essentially no time and the profile would show nothing.
func busyWork(iterations int) uint64 {
	var acc uint64
	for i := range iterations {
		acc += uint64(i)*2 + 1
	}
	return acc
}

// BusyWork is the exported spelling of busyWork so the demo (a separate package)
// can call it. Real code keeps this unexported inside the store; it is exported
// here only to demonstrate the helper on its own.
func BusyWork(iterations int) uint64 { return busyWork(iterations) }

// sink holds a busyWork result so the optimizer cannot prove it is unused.
var sink uint64

// guard observes a busyWork result on a branch that never fires, defeating
// dead-code elimination inside a locked region without changing behavior.
func guard(v uint64) {
	if v == 1<<63 {
		sink = v
	}
}

// UnderLock models the shape of a real critical section: mutate, then do work the
// compiler cannot elide. It returns the value so callers and tests can observe it.
func UnderLock(iterations int) uint64 {
	v := busyWork(iterations)
	guard(v)
	return v
}
```

### The runnable demo

The demo prints `busyWork` sums for a few sizes so you can see the helper produces
deterministic, non-zero output — proof the work is real and observable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/busywork"
)

func main() {
	for _, n := range []int{0, 1, 4, 64} {
		fmt.Printf("busyWork(%d)=%d\n", n, busywork.BusyWork(n))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
busyWork(0)=0
busyWork(1)=1
busyWork(4)=16
busyWork(64)=4096
```

### Tests

`TestBusyWorkArithmetic` pins the closed form: summing `2*i+1` for `i` in `[0, n)`
is exactly `n*n`, so `busyWork(n) == n*n`. That both documents the helper and, by
reading the return value, guarantees the compiler cannot elide it in the test.
`BenchmarkBusyWork` reports ns/op via `b.ReportMetric` so a reviewer can confirm
the helper does measurable, non-zero work — if it ever benchmarked at ~0 ns/op, the
loop was optimized out and the contention signal would be gone.

Create `busy_test.go`:

```go
package busywork

import "testing"

func TestBusyWorkArithmetic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int
		want uint64
	}{
		{0, 0},
		{1, 1},
		{4, 16},
		{10, 100},
		{64, 4096},
	}
	for _, c := range cases {
		if got := busyWork(c.n); got != c.want {
			t.Errorf("busyWork(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

func TestUnderLockObservable(t *testing.T) {
	t.Parallel()
	if got := UnderLock(64); got != 4096 {
		t.Fatalf("UnderLock(64) = %d, want 4096", got)
	}
}

func BenchmarkBusyWork(b *testing.B) {
	var total uint64
	for range b.N {
		total += busyWork(64)
	}
	// Report per-op work so a reviewer can confirm it is non-zero. Consume total
	// so the loop cannot be elided.
	b.ReportMetric(float64(total%7), "checksum")
}
```

## Review

The helper is correct when its result is observable everywhere it is called: the
demo prints it, the test asserts it, and inside a locked region `guard` consumes it
on an impossible branch. The single mistake this exercise exists to prevent is
letting the work be optimized away — a `busyWork` whose result is discarded
compiles to nothing, the lock is held for zero time, and every downstream mutex
profile comes back empty for reasons that look like a profiler bug but are actually
a compiler doing its job. Confirm the benchmark reports a non-trivial ns/op; if it
ever reads ~0, the loop was elided and the contention signal is gone. The closed
form `busyWork(n) == n*n` is the cheap invariant that keeps the helper honest.

## Resources

- [testing.B.ReportMetric](https://pkg.go.dev/testing#B.ReportMetric) — attaching custom per-op metrics to a benchmark.
- [Benchmarks (testing package)](https://pkg.go.dev/testing#hdr-Benchmarks) — how `b.N` and the timer work.
- [Profiling Go Programs](https://go.dev/blog/pprof) — why the work under a lock has to be real for a profile to show it.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-sharded-store.md](02-sharded-store.md) | Next: [04-sharded-vs-single-benchmark.md](04-sharded-vs-single-benchmark.md)
