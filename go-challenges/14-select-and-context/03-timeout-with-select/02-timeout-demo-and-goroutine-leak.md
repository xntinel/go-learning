# Exercise 2: Proving the leak — unbuffered vs buffered result channels

The timeout primitives from Exercise 1 hide a failure that only shows up under
load. When a timeout wins the race, the producer goroutine is still running and
about to send its result. If it sends on an *unbuffered* channel nobody reads, it
blocks forever: one leaked goroutine per timeout. This module ships a leaky
variant and a fixed variant side by side, and a test that counts goroutines to
prove which one leaks.

## What you'll build

```text
leakdemo/                     module example.com/leakdemo
  go.mod
  leakdemo.go                 ErrTimeout; LeakyCall; SafeCall
  cmd/demo/main.go            leaky timeout, safe timeout, fast success
  leakdemo_test.go            NumGoroutine leak proof and no-leak proof
```

Files: `leakdemo.go`, `cmd/demo/main.go`, `leakdemo_test.go`.
Implement: `LeakyCall(op, budget)` on an unbuffered result channel; `SafeCall(op, budget)` on a buffered size-1 channel.
Test: N leaky calls that time out leave goroutines elevated; N safe calls return the count to baseline; both under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/03-timeout-with-select/02-timeout-demo-and-goroutine-leak/cmd/demo
cd go-solutions/14-select-and-context/03-timeout-with-select/02-timeout-demo-and-goroutine-leak
```

### The only difference that matters

`LeakyCall` and `SafeCall` are byte-for-byte identical except for one thing: the
capacity of the result channel. That single character is the whole lesson.

In `LeakyCall` the channel is unbuffered, `make(chan int)`. The producer goroutine
runs `op()`, which is slow, and then attempts `res <- v`. By the time it gets
there, the `select` has already fired its timeout case and the caller has
returned. There is no reader left. `res <- v` on an unbuffered channel needs a
simultaneous receiver, and none will ever come, so the goroutine parks on that
send forever. Multiply by every timed-out request and you have a goroutine leak
that grows without bound.

In `SafeCall` the channel is `make(chan int, 1)`. The single buffer slot means the
producer's send succeeds immediately even with no reader: the value lands in the
buffer, the send returns, and the goroutine runs to completion and exits. Nobody
ever reads that buffered value — it is garbage-collected with the channel — but
the goroutine is *gone*, not leaked. There is also no data race: the value is
written into the channel and never read, and channel operations are synchronized,
so `-race` stays clean.

This is why every "spawn a goroutine and race it against a timer" pattern must use
a buffered result channel. The buffer is not an optimization; it is the exit door
for the loser of the race.

Create `leakdemo.go`:

```go
package leakdemo

import (
	"errors"
	"time"
)

// ErrTimeout is returned when a call exceeds its budget.
var ErrTimeout = errors.New("leakdemo: operation timed out")

// LeakyCall runs op in a goroutine and races it against budget, using an
// UNBUFFERED result channel. When the timeout wins, op's goroutine blocks forever
// on its send: a goroutine leak. Shown for contrast; do not copy this.
func LeakyCall(op func() int, budget time.Duration) (int, error) {
	res := make(chan int) // unbuffered: the loser of the race can never send
	go func() {
		res <- op() // blocks forever if the timeout already fired
	}()
	select {
	case v := <-res:
		return v, nil
	case <-time.After(budget):
		return 0, ErrTimeout
	}
}

// SafeCall is identical to LeakyCall except the result channel has a buffer of 1,
// so the abandoned producer can complete its send and exit cleanly.
func SafeCall(op func() int, budget time.Duration) (int, error) {
	res := make(chan int, 1) // buffered: the loser of the race sends and returns
	go func() {
		res <- op()
	}()
	select {
	case v := <-res:
		return v, nil
	case <-time.After(budget):
		return 0, ErrTimeout
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

	"example.com/leakdemo"
)

func main() {
	slow := func() int {
		time.Sleep(100 * time.Millisecond)
		return 42
	}

	_, err := leakdemo.LeakyCall(slow, 10*time.Millisecond)
	fmt.Printf("leaky: timedOut=%v (a goroutine is now leaked)\n", errors.Is(err, leakdemo.ErrTimeout))

	_, err = leakdemo.SafeCall(slow, 10*time.Millisecond)
	fmt.Printf("safe:  timedOut=%v (producer will finish and exit)\n", errors.Is(err, leakdemo.ErrTimeout))

	v, err := leakdemo.SafeCall(func() int { return 7 }, time.Second)
	fmt.Printf("fast:  v=%d err=%v\n", v, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leaky: timedOut=true (a goroutine is now leaked)
safe:  timedOut=true (producer will finish and exit)
fast:  v=7 err=<nil>
```

### Tests

The proof is a goroutine census. `TestLeakyCallLeaks` records the baseline
goroutine count, fires N leaky calls that all time out against a slow producer,
waits long enough for every producer to reach its blocked send, and asserts the
count stayed elevated — the goroutines are stuck, exactly as designed to
demonstrate. `TestSafeCallNoLeak` does the same with the buffered variant and
polls until the count returns to baseline, proving the producers drained and
exited. Both are deliberately *not* `t.Parallel()`: they read a global counter and
must not run concurrently with other tests. Because they are sequential, the leaked
goroutines from the first test simply sit inside the second test's freshly measured
baseline and do not confuse it.

Create `leakdemo_test.go`:

```go
package leakdemo

import (
	"errors"
	"runtime"
	"testing"
	"time"
)

func slowProducer() int {
	time.Sleep(50 * time.Millisecond)
	return 1
}

func TestLeakyCallLeaks(t *testing.T) {
	base := runtime.NumGoroutine()
	const n = 20
	for range n {
		if _, err := LeakyCall(slowProducer, 5*time.Millisecond); !errors.Is(err, ErrTimeout) {
			t.Fatalf("LeakyCall err = %v, want ErrTimeout", err)
		}
	}
	// Give every producer time to finish its sleep and reach the blocked send.
	time.Sleep(200 * time.Millisecond)
	leaked := runtime.NumGoroutine() - base
	if leaked < n/2 {
		t.Fatalf("expected ~%d leaked goroutines, got %d", n, leaked)
	}
}

func TestSafeCallNoLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	const n = 20
	for range n {
		if _, err := SafeCall(slowProducer, 5*time.Millisecond); !errors.Is(err, ErrTimeout) {
			t.Fatalf("SafeCall err = %v, want ErrTimeout", err)
		}
	}
	// Poll until the producers drain into their buffers and exit.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if extra := runtime.NumGoroutine() - base; extra > 2 {
		t.Fatalf("expected goroutines to return to baseline, %d still live", extra)
	}
}
```

## Review

The library is correct in the narrow sense that both variants time out and return
`ErrTimeout`; the point of the exercise is that only `SafeCall` is correct in the
sense that matters in production. The leak is invisible to a functional test — both
return the right error — which is exactly why it survives to production and why a
goroutine census is the right instrument. The mistake to avoid is treating the
buffer as a style preference: an unbuffered result channel behind a timeout is a
latent goroutine leak, full stop. Run `go test -race` to confirm the discarded
buffered value in `SafeCall` is written and never read without a data race, and
watch `TestLeakyCallLeaks` document the failure while `TestSafeCallNoLeak` proves
the fix.

## Resources

- [`runtime.NumGoroutine`](https://pkg.go.dev/runtime#NumGoroutine) — the census instrument used to detect the leak.
- [Go Memory Model: channel communication](https://go.dev/ref/mem#chan) — why the buffered send synchronizes and does not race.
- [Effective Go: channels](https://go.dev/doc/effective_go#channels) — buffered vs unbuffered semantics.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-timeout-primitives.md](01-timeout-primitives.md) | Next: [03-reset-timer-idle-consumer.md](03-reset-timer-idle-consumer.md)
