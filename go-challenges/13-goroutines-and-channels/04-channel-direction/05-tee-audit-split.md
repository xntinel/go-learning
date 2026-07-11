# Exercise 5: Tee — Duplicate One Stream Into a Processor and an Audit Log

Splitting one stream into two — the primary processor and a compliance audit sink
— is the tee pattern. `Tee(in <-chan T) (<-chan T, <-chan T)` forwards every
value to both outputs. The correctness trap is that a slow consumer on one output
must not silently stall or drop values on the other; the fix is the nil-channel
select trick.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
tee/                         independent module: example.com/tee
  go.mod                     go 1.26
  tee.go                     Tee[T any](in <-chan T) (<-chan T, <-chan T)
  cmd/
    demo/
      main.go                runnable demo: process + audit the same stream
  tee_test.go                same sequence to both, both close, backpressure
```

Files: `tee.go`, `cmd/demo/main.go`, `tee_test.go`.
Implement: generic `Tee(in <-chan T) (<-chan T, <-chan T)` forwarding each value to both outputs exactly once, using the nil-channel select trick, closing both when `in` closes.
Test: both outputs receive the same ordered sequence, both close, and a deliberately slow second consumer still receives every value.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tee/cmd/demo
cd ~/go-exercises/tee
go mod init example.com/tee
```

### Why the nil-channel select trick

`Tee` reads each value from `in` and must deliver it to *both* `out1` and `out2`
before moving to the next value — otherwise the two outputs could drift out of
sync. The naive approach, "send to out1, then send to out2," is a trap: if the
`out1` consumer is slow, the send to `out1` blocks, and `out2` never sees the
value until `out1` catches up. One slow consumer stalls the other, which for an
audit split means the audit log silently falls behind the processor.

The fix uses two facts about `select`: a `select` picks whichever send is ready
first, and a send on a `nil` channel blocks forever (so a `nil` case is disabled).
For each value, `Tee` copies `out1` and `out2` into local variables and loops
twice. Each iteration `select`s between sending to whichever output is still live;
after a successful send, it sets that local to `nil`, disabling its case. The
second iteration can then only send to the other output. The result: each value
goes to both outputs exactly once, in whichever order each consumer happens to be
ready, and neither consumer's pace gates the other's correctness.

When `in` closes, the `for range in` loop ends and the deferred closes fire,
closing both outputs. Both `defer close` calls run, so both outputs are always
closed exactly once when the input drains.

One operational note: the outputs are unbuffered, so a consumer that never reads
one output *will* eventually block the tee (there is no free lunch on
backpressure). The nil-select trick fixes *ordering* independence between two
*active* consumers; it does not let you ignore an output entirely. Tests must
drain both outputs, in separate goroutines, to avoid deadlock.

Create `tee.go`:

```go
package tee

// Tee forwards every value from in to both returned outputs. Each value is
// delivered to both outputs exactly once; the nil-channel select trick means a
// slow consumer on one output does not block delivery to the other. Both
// outputs are closed when in closes.
func Tee[T any](in <-chan T) (<-chan T, <-chan T) {
	out1 := make(chan T)
	out2 := make(chan T)
	go func() {
		defer close(out1)
		defer close(out2)
		for v := range in {
			a, b := out1, out2
			for range 2 {
				select {
				case a <- v:
					a = nil
				case b <- v:
					b = nil
				}
			}
		}
	}()
	return out1, out2
}
```

Setting `a = nil` after a successful send disables that case on the next loop
iteration, so the second iteration is forced to send on the still-live channel.
Two iterations guarantee both outputs received the value.

### The runnable demo

The demo tees a stream of order IDs into a processor and an audit log, draining
both concurrently and joining on a `sync.WaitGroup`. Each side sorts nothing —
delivery to each output is ordered — so both print the same sequence.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/tee"
)

func source(vals ...int) <-chan int {
	ch := make(chan int)
	go func() {
		defer close(ch)
		for _, v := range vals {
			ch <- v
		}
	}()
	return ch
}

func main() {
	process, audit := tee.Tee(source(101, 102, 103))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var got []int
		for v := range process {
			got = append(got, v)
		}
		fmt.Printf("processor saw: %v\n", got)
	}()
	go func() {
		defer wg.Done()
		var got []int
		for v := range audit {
			got = append(got, v)
		}
		fmt.Printf("audit saw:     %v\n", got)
	}()
	wg.Wait()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processor saw: [101 102 103]
audit saw:     [101 102 103]
```

### Tests

Both outputs are drained in separate goroutines to avoid deadlock.
`TestTeeDeliversSameSequenceToBoth` asserts both collected slices are identical
and ordered. `TestTeeClosesBothOutputs` asserts both channels close after the
input drains. `TestTeeBackpressure` puts a deliberate delay on the second
consumer and asserts it still receives every value — proving the nil-select trick
keeps the slow side from dropping anything.

Create `tee_test.go`:

```go
package tee

import (
	"slices"
	"sync"
	"testing"
	"time"
)

func source(vals ...int) <-chan int {
	ch := make(chan int)
	go func() {
		defer close(ch)
		for _, v := range vals {
			ch <- v
		}
	}()
	return ch
}

func drainBoth(a, b <-chan int) (ra, rb []int) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for v := range a {
			ra = append(ra, v)
		}
	}()
	go func() {
		defer wg.Done()
		for v := range b {
			rb = append(rb, v)
		}
	}()
	wg.Wait()
	return ra, rb
}

func TestTeeDeliversSameSequenceToBoth(t *testing.T) {
	t.Parallel()

	want := []int{1, 2, 3, 4, 5}
	a, b := Tee(source(want...))
	ra, rb := drainBoth(a, b)
	if !slices.Equal(ra, want) {
		t.Fatalf("output A = %v, want %v", ra, want)
	}
	if !slices.Equal(rb, want) {
		t.Fatalf("output B = %v, want %v", rb, want)
	}
}

func TestTeeClosesBothOutputs(t *testing.T) {
	t.Parallel()

	a, b := Tee(source(1))
	drainBoth(a, b)
	if _, ok := <-a; ok {
		t.Fatal("output A not closed after input drained")
	}
	if _, ok := <-b; ok {
		t.Fatal("output B not closed after input drained")
	}
}

func TestTeeBackpressure(t *testing.T) {
	t.Parallel()

	want := []int{10, 20, 30}
	a, b := Tee(source(want...))

	var ra, rb []int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for v := range a {
			ra = append(ra, v)
		}
	}()
	go func() {
		defer wg.Done()
		for v := range b {
			time.Sleep(time.Millisecond) // deliberately slow consumer
			rb = append(rb, v)
		}
	}()
	wg.Wait()

	if !slices.Equal(ra, want) {
		t.Fatalf("fast consumer = %v, want %v", ra, want)
	}
	if !slices.Equal(rb, want) {
		t.Fatalf("slow consumer dropped values: %v, want %v", rb, want)
	}
}
```

## Review

The tee is correct when both outputs receive every value in order and both close
when the input closes. The backpressure test is the meaningful one: a sequential
"send A then send B" tee would pass the first two tests and still let a slow
consumer stall the other output — the nil-select trick is what makes the slow
consumer merely slow, not lossy. Because both outputs are unbuffered, always
drain both in separate goroutines; draining only one deadlocks the tee. Run
`go test -race` to confirm the concurrent drains are clean.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the tee/fan-out pattern and channel duplication.
- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — how `select` chooses a ready case and how a `nil` channel case is skipped.
- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — the receive-only inputs and outputs that make the tee's contract explicit.

---

Prev: [04-fan-in-merge.md](04-fan-in-merge.md) | Back to [00-concepts.md](00-concepts.md) | Next: [06-or-done-cancellation.md](06-or-done-cancellation.md)
