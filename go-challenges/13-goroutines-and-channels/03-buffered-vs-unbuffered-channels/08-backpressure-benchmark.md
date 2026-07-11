# Exercise 8: Measuring Backpressure: Unbuffered vs Right-Sized vs Oversized Buffer

The concepts section claims three things about capacity: an unbuffered channel is a
lockstep synchronization handoff, a right-sized buffer decouples up to its capacity,
and an oversized buffer buys latent memory and hidden latency with no correctness gain.
This exercise pins all three with evidence — a deterministic test that proves the
unbuffered send blocks until a receiver is ready, and a benchmark that shows results are
identical across capacities so oversizing changes only memory, never the output.

This module is fully self-contained.

## What you'll build

```text
backpressure/                module: example.com/backpressure
  go.mod                     go 1.26
  backpressure.go            Run(capacity, n) []int; IsOrdered helper
  cmd/
    demo/
      main.go                run cap 0 / burst / oversized, show identical ordered output
  backpressure_test.go       unbuffered-blocks proof, cap-1-proceeds proof, capacity-independence, no-leak, benchmark
```

- Files: `backpressure.go`, `cmd/demo/main.go`, `backpressure_test.go`.
- Implement: `Run(capacity, n int) []int` — one producer sends `0..n-1` into a channel of the given capacity, one consumer drains it; `IsOrdered([]int) bool`.
- Test: a lone send on an unbuffered channel blocks until a receiver is ready; a cap-1 channel lets one send proceed with no receiver; `Run` yields the same ordered result for every capacity; no goroutine leak; a benchmark across capacities.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/backpressure/cmd/demo
cd ~/go-exercises/backpressure
go mod init example.com/backpressure
go mod edit -go=1.26
```

### What the evidence actually shows

`Run(capacity, n)` is deliberately the *same* code for every capacity — one producer,
one consumer, a channel of the given size. The producer sends `0..n-1` in order and
closes; the consumer ranges and collects. Because there is exactly one producer and one
consumer, the output is `[0, 1, ..., n-1]` in order *regardless of capacity*. That is the
first piece of evidence: capacity does not change the result. An oversized buffer
produces the identical slice a right-sized or unbuffered channel does — the only thing
it changes is how much memory the channel's ring buffer holds and how long a value can
linger in it before the consumer takes it. Bigger is not more correct; it is only more
memory.

The blocking behavior is the second piece. An unbuffered send cannot complete until a
receiver is ready to take the value — that is the happens-before handoff, the barrier.
The test proves it directly: launch a goroutine that sends on an unbuffered channel and
closes a `sent` signal *after* the send returns; observe that `sent` stays open for a
window (the send is blocked, no receiver yet); then receive, and watch `sent` close (the
send completed only once the receive happened). A cap-1 channel behaves differently: the
lone send drops into the buffer and returns immediately with no receiver present —
decoupling up to capacity. Same harness, opposite outcome, because the capacity is the
only thing that changed.

The benchmark reports items/sec across `unbuffered`, `cap-burst`, and `cap-oversized`.
Its point is *not* an absolute number (those are CI-unstable and machine-dependent);
it is that the three variants do the same work and produce the same output, so the only
axis on which the oversized one "wins" is memory footprint, on which it loses. The test
suite asserts the capacity-independence of the *result*; the benchmark is documentation
you run by hand with `go test -bench`. A goroutine-leak test uses `runtime.NumGoroutine`
to confirm the producer goroutines exit after the channel closes.

Create `backpressure.go`:

```go
package backpressure

// Run sends 0..n-1 from one producer into a channel of the given capacity and drains
// it with one consumer. The result is [0, 1, ..., n-1] for every capacity: capacity
// changes memory and latency, never the output.
func Run(capacity, n int) []int {
	ch := make(chan int, capacity) // capacity 0 => unbuffered
	go func() {
		defer close(ch)
		for i := range n {
			ch <- i
		}
	}()

	out := make([]int, 0, n)
	for v := range ch {
		out = append(out, v)
	}
	return out
}

// IsOrdered reports whether s is exactly 0, 1, ..., len(s)-1.
func IsOrdered(s []int) bool {
	for i, v := range s {
		if v != i {
			return false
		}
	}
	return true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/backpressure"
)

func main() {
	const n = 8
	for _, capacity := range []int{0, 8, 1024} {
		out := backpressure.Run(capacity, n)
		fmt.Printf("cap=%-4d len=%d ordered=%v\n", capacity, len(out), backpressure.IsOrdered(out))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cap=0    len=8 ordered=true
cap=8    len=8 ordered=true
cap=1024 len=8 ordered=true
```

### Tests

`TestUnbufferedSendBlocksUntilReceiver` proves the barrier: the `sent` signal stays open
while the send has no receiver, and closes only after the receive. `TestCap1SendProceeds`
proves decoupling: a lone send into a cap-1 buffer returns with no receiver.
`TestResultsIndependentOfCapacity` runs `Run` across capacities and asserts every result
equals the ordered `0..n-1` — the correctness-does-not-depend-on-capacity claim.
`TestNoGoroutineLeak` confirms producer goroutines exit. `BenchmarkThroughput` compares
capacities but is not asserted on timing.

Create `backpressure_test.go`:

```go
package backpressure

import (
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestUnbufferedSendBlocksUntilReceiver(t *testing.T) {
	t.Parallel()

	ch := make(chan int)
	sent := make(chan struct{})
	go func() {
		ch <- 1 // blocks until a receiver is ready
		close(sent)
	}()

	select {
	case <-sent:
		t.Fatal("unbuffered send completed with no receiver ready")
	case <-time.After(20 * time.Millisecond):
		// expected: still blocked on the handoff
	}

	if v := <-ch; v != 1 {
		t.Fatalf("received %d, want 1", v)
	}

	select {
	case <-sent:
		// expected: the send completed once the receive happened
	case <-time.After(time.Second):
		t.Fatal("send did not complete after the receive")
	}
}

func TestCap1SendProceeds(t *testing.T) {
	t.Parallel()

	ch := make(chan int, 1)
	sent := make(chan struct{})
	go func() {
		ch <- 1 // drops into the buffer, no receiver needed
		close(sent)
	}()

	select {
	case <-sent:
		// expected: decoupled up to capacity
	case <-time.After(time.Second):
		t.Fatal("send into an empty cap-1 buffer blocked")
	}
}

func TestResultsIndependentOfCapacity(t *testing.T) {
	t.Parallel()

	const n = 200
	want := make([]int, n)
	for i := range n {
		want[i] = i
	}
	for _, capacity := range []int{0, 1, n, 1 << 14} {
		got := Run(capacity, n)
		if !slices.Equal(got, want) {
			t.Fatalf("Run(cap=%d) result differs from ordered baseline", capacity)
		}
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	t.Parallel()

	before := runtime.NumGoroutine()
	for range 50 {
		Run(4, 100)
	}
	for range 200 {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("goroutines leaked: before=%d after=%d", before, runtime.NumGoroutine())
}

func BenchmarkThroughput(b *testing.B) {
	const n = 1000
	for _, tc := range []struct {
		name string
		cap  int
	}{
		{"unbuffered", 0},
		{"cap-burst", n},
		{"cap-oversized", 1 << 16},
	} {
		b.Run(tc.name, func(b *testing.B) {
			start := time.Now()
			for range b.N {
				Run(tc.cap, n)
			}
			if elapsed := time.Since(start); elapsed > 0 {
				b.ReportMetric(float64(b.N*n)/elapsed.Seconds(), "items/sec")
			}
		})
	}
}

func ExampleRun() {
	fmt.Println(Run(0, 5))
	// Output: [0 1 2 3 4]
}
```

## Review

The evidence is the point. `TestResultsIndependentOfCapacity` proves capacity does not
change the output, so oversizing a buffer buys nothing but memory and latency — exactly
the concepts-section claim, now backed by a passing test. `TestUnbufferedSendBlocksUntilReceiver`
proves the unbuffered channel is a barrier: the send does not complete until the receive,
which is the happens-before handoff you choose an unbuffered channel *for*.
`TestCap1SendProceeds` shows a single buffer slot decouples one send. Keep the benchmark
unasserted on absolute timing — those numbers are machine- and load-dependent and would
make the suite flaky; the benchmark exists to be read, not gated. The mistake to avoid is
concluding from a faster benchmark that a bigger buffer is "better"; at steady state
throughput is bounded by the consumer, and the extra capacity only hides latency.

## Resources

- [The Go Memory Model: Channel communication](https://go.dev/ref/mem#chan) — send happens-before receive; the unbuffered handoff.
- [pkg.go.dev: testing.B.ReportMetric](https://pkg.go.dev/testing#B.ReportMetric) — reporting custom benchmark metrics.
- [pkg.go.dev: runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) — observing goroutine count to check for leaks.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-graceful-shutdown-drain.md](07-graceful-shutdown-drain.md) | Next: [09-errgroup-bounded-pipeline.md](09-errgroup-bounded-pipeline.md)
