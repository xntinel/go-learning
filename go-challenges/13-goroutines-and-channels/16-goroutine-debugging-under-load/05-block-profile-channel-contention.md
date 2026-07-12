# Exercise 5: Localize channel/wait contention with the block profile

When latency is lost to *waiting* rather than computing, a CPU profile is silent —
it only shows where cycles burn. The block profile shows where goroutines block, and
for how long. This exercise enables block profiling, drives contention on an
unbuffered channel, and reads the profile back to name the blocking site.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
blockprof/                independent module: example.com/blockprof
  go.mod
  blockprof.go            Contend (channel contention), Profile (enable/capture/disable), BlockCount
  cmd/demo/main.go        run contention under profiling, show it names Contend
  blockprof_test.go       profile has records and names the blocking site
```

- Files: `blockprof.go`, `cmd/demo/main.go`, `blockprof_test.go`.
- Implement: `Contend(n)` driving `n` blocking sends on an unbuffered channel; `Profile(rate, fn)` that calls `runtime.SetBlockProfileRate(rate)`, runs `fn`, captures `pprof.Lookup("block")` as text (debug=1), then resets the rate to 0; `BlockCount()`.
- Test: with rate=1 the captured profile has `Count() > 0` and its text names `Contend`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/16-goroutine-debugging-under-load/05-block-profile-channel-contention/cmd/demo
cd go-solutions/13-goroutines-and-channels/16-goroutine-debugging-under-load/05-block-profile-channel-contention
```

### Why block profiling is off by default, and how to read it

The block profile records, for each stack, how long goroutines spent blocked there:
channel sends and receives, `select`, `sync.WaitGroup.Wait`, `sync.Cond.Wait`. It is
disabled by default because sampling every blocking event has a cost; you turn it on
to diagnose a *known* waiting problem and turn it off again. `runtime.SetBlockProfileRate(rate)`
controls sampling: the runtime records a blocking event with probability such that,
on average, one sample is taken per `rate` nanoseconds of blocking. `rate = 1`
captures essentially every event (maximum resolution, maximum overhead); `rate = 0`
disables it. There is a real trade-off here — a production diagnosis might use a
larger rate to sample cheaply — so `Profile` takes the rate as a parameter and always
resets it to 0 with `defer`.

`Contend` manufactures the contention deterministically: a producer sends `n`
integers on an *unbuffered* channel while the main goroutine receives them one at a
time. Every send blocks until the matching receive runs, so each is a blocking event
the profiler samples. Reading `pprof.Lookup("block").WriteTo(w, 1)` produces a text
profile whose stacks are symbolized — the frames name `runtime.chansend` and the
`blockprof.Contend` closure that issued the send — so a test can assert the profile
points at the blocking site rather than merely counting samples.

The block profile is process-global and cumulative, so these functions manipulate a
shared switch. Do not run block-profiling tests in parallel with each other: one
test's `defer SetBlockProfileRate(0)` would disable another's sampling mid-run.

Create `blockprof.go`:

```go
package blockprof

import (
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
)

// Contend drives n blocking sends on an unbuffered channel: a producer sends
// while the caller's goroutine receives one at a time, so every send blocks and
// is sampled by the block profiler.
func Contend(n int) {
	ch := make(chan int)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range n {
			ch <- i // blocks until the receive below runs
		}
	}()
	for range n {
		<-ch
	}
	wg.Wait()
}

// Profile enables block profiling at rate, runs fn, captures the block profile as
// text (debug=1), then disables profiling. rate is roughly one sample per rate
// nanoseconds spent blocked; 1 samples essentially every blocking event.
func Profile(rate int, fn func()) string {
	runtime.SetBlockProfileRate(rate)
	defer runtime.SetBlockProfileRate(0)
	fn()
	var b strings.Builder
	_ = pprof.Lookup("block").WriteTo(&b, 1)
	return b.String()
}

// BlockCount reports the number of distinct blocking stacks recorded so far.
func BlockCount() int {
	return pprof.Lookup("block").Count()
}
```

### The runnable demo

The demo profiles a run of `Contend` at rate 1 and prints two derived facts: that the
block profile recorded at least one stack, and that its text names the `Contend`
site.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/blockprof"
)

func main() {
	prof := blockprof.Profile(1, func() {
		blockprof.Contend(1000)
	})
	fmt.Println("block records > 0:", blockprof.BlockCount() > 0)
	fmt.Println("names Contend:", strings.Contains(prof, "Contend"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
block records > 0: true
names Contend: true
```

### Tests

`TestBlockProfileCapturesContention` drives contention under rate 1 and asserts the
profile has records and its text names the blocking function. The test is not marked
parallel because it toggles the process-global block-profile rate.

Create `blockprof_test.go`:

```go
package blockprof

import (
	"fmt"
	"strings"
	"testing"
)

func TestBlockProfileCapturesContention(t *testing.T) {
	prof := Profile(1, func() {
		Contend(2000)
	})

	if BlockCount() == 0 {
		t.Fatal("block profile has no records; expected channel contention")
	}
	if !strings.Contains(prof, "Contend") {
		t.Errorf("block profile does not name the contended site:\n%s", prof)
	}
}

func TestProfileNamesChannelOp(t *testing.T) {
	prof := Profile(1, func() {
		Contend(1000)
	})
	if !strings.Contains(prof, "chan") {
		t.Errorf("expected the profile to reference a channel operation:\n%s", prof)
	}
}

func ExampleProfile() {
	prof := Profile(1, func() {
		Contend(500)
	})
	fmt.Println(strings.Contains(prof, "Contend"))
	// Output: true
}
```

## Review

The profile is correct when it both has records and names the site where goroutines
blocked — a count alone would not tell you *where*. The determinism comes from the
unbuffered channel: an unbuffered send cannot complete until a receiver is ready, so
`Contend` guarantees `n` blocking events rather than hoping the scheduler produces
some. The two switches to respect: block profiling must be *enabled* before the
workload, or the profile is empty and you would wrongly conclude there is no
contention; and it must be reset to 0 afterward, which the `defer` in `Profile`
guarantees, so the process does not keep paying sampling overhead. Because the
profile is global and cumulative, these tests run sequentially. Run `go test -race`
to confirm the producer/consumer handoff is clean.

## Resources

- [`runtime.SetBlockProfileRate`](https://pkg.go.dev/runtime#SetBlockProfileRate) — enabling block profiling and the meaning of the rate.
- [`runtime/pprof.Lookup`](https://pkg.go.dev/runtime/pprof#Lookup) and [`Profile.WriteTo`](https://pkg.go.dev/runtime/pprof#Profile.WriteTo) — reading the block profile as protobuf (debug=0) or text (debug=1).
- [Go Blog: Profiling Go Programs](https://go.dev/blog/pprof) — how the profiles fit together and when to use each.

---

Prev: [04-pprof-labels-attribute-goroutines.md](04-pprof-labels-attribute-goroutines.md) | Back to [00-concepts.md](00-concepts.md) | Next: [06-mutex-profile-lock-contention.md](06-mutex-profile-lock-contention.md)
