# Exercise 1: Bounded receives — a reusable timeout primitives library

Every timeout in a Go backend reduces to one move: put a data channel and a timer
channel in the same `select`. This exercise packages that move into three generic
primitives a worker can lean on — a one-shot bounded receive, a drain with a
budget, and a loop bounded by an overall deadline — each reporting a shared
`ErrTimeout` sentinel so a caller can tell a budget breach apart from a closed
queue.

## What you'll build

```text
timeoutprims/                 module example.com/timeouts
  go.mod
  timeouts.go                 ErrTimeout; ReceiveWithTimeout[T]; DrainWithTimeout[T]; DeadlineLoop[T]
  cmd/demo/main.go            fast hit, slow timeout, drain-then-timeout
  timeouts_test.go            contract tests pinning success/timeout/closed states + Example
```

Files: `timeouts.go`, `cmd/demo/main.go`, `timeouts_test.go`.
Implement: `ReceiveWithTimeout[T](ch, d)`, `DrainWithTimeout[T](ch, d)`, `DeadlineLoop[T](ch, d)`, and the `ErrTimeout` sentinel.
Test: buffered channel yields under budget; unbuffered times out after ~budget; closed channel times out; loop stops on close (nil) and on deadline (ErrTimeout); drain collects buffered values then times out.
Verify: `go test -count=1 -race ./...`

### Why three primitives, one sentinel

The three functions answer three distinct production questions. `ReceiveWithTimeout`
is the one-shot: "wait up to `d` for the next value, then give up." A worker
pulling from a job channel uses it to avoid blocking forever on an idle queue.
`DeadlineLoop` bounds an *entire* consumption loop with a single timer created
once, outside the loop: "consume values until the producer closes the channel, but
never run longer than `d` in total." `DrainWithTimeout` is the drain shape: collect
every value that arrives, and return what you have once a total budget of `d`
elapses since the drain began.

All three return the same `ErrTimeout`. That matters because the caller's branch
is the same regardless of which primitive raised it: a budget breach is a budget
breach. The one subtlety worth internalizing is how a *closed* channel is handled.
A receive on a closed channel returns immediately with `ok == false` and the zero
value. For `ReceiveWithTimeout`, that is not a delivered value — there will never
be one — so it too returns `ErrTimeout`: the semantics are "I did not get a value
within the budget," and a closed empty channel guarantees you never will. For
`DeadlineLoop` and `DrainWithTimeout`, a close is the *normal* end of the stream,
so they return the accumulated result with a `nil` error.

`DeadlineLoop` shows the overall-deadline pattern in its cleanest form: one
`time.NewTimer(overall)` before the loop, `defer deadline.Stop()` to release its
runtime entry, and a `case <-deadline.C` that ends the whole loop when it fires.
`DrainWithTimeout` uses `time.After` because it never resets — a single one-shot
budget for the whole drain is exactly what `time.After` gives, and there is no loop
allocation problem because it is evaluated once.

Create `timeouts.go`:

```go
package timeouts

import (
	"errors"
	"time"
)

// ErrTimeout is returned when a call exceeds its budget. Callers compare with
// errors.Is, never by matching the message.
var ErrTimeout = errors.New("timeouts: operation timed out")

// ReceiveWithTimeout waits up to timeout for a value on ch. A closed channel
// yields no value, so it too reports ErrTimeout.
func ReceiveWithTimeout[T any](ch <-chan T, timeout time.Duration) (T, error) {
	var zero T
	select {
	case v, ok := <-ch:
		if !ok {
			return zero, ErrTimeout
		}
		return v, nil
	case <-time.After(timeout):
		return zero, ErrTimeout
	}
}

// DeadlineLoop reads values from ch until it is closed or the overall budget
// fires. One timer, created once outside the loop, bounds the whole operation.
func DeadlineLoop[T any](ch <-chan T, overall time.Duration) (int, error) {
	deadline := time.NewTimer(overall)
	defer deadline.Stop()

	count := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return count, nil
			}
			count++
		case <-deadline.C:
			return count, ErrTimeout
		}
	}
}

// DrainWithTimeout collects every value from ch until it is closed or a total
// budget of timeout elapses since the drain began. Values collected so far are
// always returned.
func DrainWithTimeout[T any](ch <-chan T, timeout time.Duration) ([]T, error) {
	var out []T
	deadline := time.After(timeout)
	for {
		select {
		case v, ok := <-ch:
			if !ok {
				return out, nil
			}
			out = append(out, v)
		case <-deadline:
			return out, ErrTimeout
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/timeouts"
)

func main() {
	fast := make(chan int, 1)
	fast <- 1
	v, err := timeouts.ReceiveWithTimeout(fast, time.Second)
	fmt.Printf("fast: v=%d err=%v\n", v, err)

	slow := make(chan int)
	go func() {
		time.Sleep(500 * time.Millisecond)
		slow <- 99
	}()
	v, err = timeouts.ReceiveWithTimeout(slow, 30*time.Millisecond)
	fmt.Printf("slow: v=%d timedOut=%v\n", v, errors.Is(err, timeouts.ErrTimeout))

	ch := make(chan int)
	go func() {
		ch <- 10
		ch <- 20
	}()
	got, err := timeouts.DrainWithTimeout(ch, 30*time.Millisecond)
	fmt.Printf("drain: %v timedOut=%v\n", got, errors.Is(err, timeouts.ErrTimeout))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast: v=1 err=<nil>
slow: v=0 timedOut=true
drain: [10 20] timedOut=true
```

### Tests

The tests pin the contract of each primitive across its states. `TestReceive*`
covers the three one-shot outcomes: a buffered value under budget succeeds; an
unbuffered channel with no sender times out after roughly the budget (the elapsed
lower bound proves it actually waited); a closed channel returns `ErrTimeout`.
`TestDeadlineLoop*` covers the two loop exits: producer close returns `nil`, and
the overall timer returns `ErrTimeout` after collecting a count inside a tolerance
band. `TestDrain*` proves the drain collects every buffered value then times out
when the producer stalls. The `Example` is verified by its `// Output:` line.

Create `timeouts_test.go`:

```go
package timeouts

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestReceiveWithTimeoutSuccess(t *testing.T) {
	t.Parallel()
	ch := make(chan int, 1)
	ch <- 7
	v, err := ReceiveWithTimeout(ch, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if v != 7 {
		t.Fatalf("v = %d, want 7", v)
	}
}

func TestReceiveWithTimeoutFires(t *testing.T) {
	t.Parallel()
	ch := make(chan int)
	start := time.Now()
	_, err := ReceiveWithTimeout(ch, 30*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed < 25*time.Millisecond {
		t.Fatalf("returned in %v, want it to wait ~30ms", elapsed)
	}
}

func TestReceiveWithTimeoutClosedChannel(t *testing.T) {
	t.Parallel()
	ch := make(chan int)
	close(ch)
	_, err := ReceiveWithTimeout(ch, time.Second)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("closed channel: err = %v, want ErrTimeout", err)
	}
}

func TestDeadlineLoopStopsOnClose(t *testing.T) {
	t.Parallel()
	ch := make(chan int, 5)
	for i := range 3 {
		ch <- i
	}
	close(ch)
	n, err := DeadlineLoop(ch, time.Second)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if n != 3 {
		t.Fatalf("n = %d, want 3", n)
	}
}

func TestDeadlineLoopStopsOnTimeout(t *testing.T) {
	t.Parallel()
	ch := make(chan int)
	stop := make(chan struct{})
	go func() {
		defer close(ch)
		for i := 0; ; i++ {
			select {
			case ch <- i:
			case <-stop:
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	n, err := DeadlineLoop(ch, 75*time.Millisecond)
	close(stop)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if n < 1 || n > 8 {
		t.Fatalf("n = %d, want roughly 3-4", n)
	}
}

func TestDrainWithTimeoutSuccess(t *testing.T) {
	t.Parallel()
	ch := make(chan string, 4)
	ch <- "a"
	ch <- "b"
	ch <- "c"
	close(ch)
	got, err := DrainWithTimeout(ch, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("%d values, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDrainWithTimeoutFiresBeforeClose(t *testing.T) {
	t.Parallel()
	ch := make(chan int)
	go func() {
		ch <- 1
		ch <- 2
	}()
	got, err := DrainWithTimeout(ch, 40*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d values, want 2", len(got))
	}
}

func ExampleReceiveWithTimeout() {
	ch := make(chan string, 1)
	ch <- "hello"
	v, err := ReceiveWithTimeout(ch, time.Second)
	if err != nil {
		fmt.Println("err:", err)
		return
	}
	fmt.Println(v)
	// Output: hello
}
```

## Review

The library is correct when each primitive's error is a pure function of what the
channel did: a delivered value gives `nil`, a budget breach gives `ErrTimeout`,
and a close gives `nil` for the stream-consuming primitives but `ErrTimeout` for
the one-shot receive that got no value. The three most common mistakes this
exercise inoculates against are comparing the timeout by string instead of
`errors.Is`, forgetting `defer deadline.Stop()` in `DeadlineLoop` (which holds the
timer's runtime entry until it fires), and confusing the per-message question with
the overall-deadline question — here `DeadlineLoop` is deliberately the overall
form, one timer outside the loop. Run `go test -race` to confirm the concurrent
producer in the loop and drain tests races cleanly.

## Resources

- [`time.After`](https://pkg.go.dev/time#After) — the one-shot timer channel these primitives select on.
- [`time.NewTimer`](https://pkg.go.dev/time#NewTimer) — the reusable timer behind `DeadlineLoop`.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — sentinel comparison for `ErrTimeout`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-timeout-demo-and-goroutine-leak.md](02-timeout-demo-and-goroutine-leak.md)
