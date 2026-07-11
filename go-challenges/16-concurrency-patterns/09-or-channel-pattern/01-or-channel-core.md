# Exercise 1: Or-Channel Core

The or-channel is the canonical way to express "wake at the first of several signals". This exercise builds it twice in one package: the flat fan-in form that most code reaches for, and the recursive pairwise-merge form that owns each merge without shared synchronization. Both satisfy the same contract — the returned channel closes when any input closes — and one test suite holds both to it.

This module is fully self-contained. It begins with its own `go mod init`, defines everything it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
orchann.go           Or (flat, sync.Once) and OrRecursive (pairwise tree merge)
cmd/
  demo/
    main.go          launch three timed closers, wait on Or, report which fired first
orchann_test.go      zero/one/nil inputs, first-fires, race close-once, recursive parity, 100 channels
```

- Files: `orchann.go`, `cmd/demo/main.go`, `orchann_test.go`.
- Implement: `Or(cs ...<-chan struct{}) <-chan struct{}` and `OrRecursive(cs ...<-chan struct{}) <-chan struct{}`.
- Test: the suite covers the zero/one/nil contracts, that the output closes when the first input fires, that closing many inputs at once never double-closes, that `OrRecursive` matches `Or`, and a 100-channel case.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p or-channel-core/cmd/demo && cd or-channel-core
go mod init example.com/or-channel-core
```

### Why two forms, and the exactly-once close

Both functions return a `<-chan struct{}` that closes when any input closes, but they reach that guarantee differently, and the difference is the lesson.

The flat `Or` spawns one goroutine per input. Each goroutine selects on two channels: its own input and the shared output. The first input to fire calls a close helper; every other goroutine then observes the closed output on its second arm and returns. Two safeguards make this correct. A `sync.Once` wraps the actual `close(out)` so that even if several inputs fire in the same scheduling slice, the channel is closed exactly once and the redundant closers are silent no-ops — without it, the second `close(out)` panics with "close of closed channel". The `<-out` arm gives every losing goroutine a guaranteed exit, so none of them leaks waiting on an input that may never fire. Nil inputs are skipped before any goroutine is spawned, because a receive on a nil channel blocks forever and would only ever leak.

The boundary cases are part of the contract. Zero inputs returns an already-closed channel, so a caller's `select` falls straight through; this is the classic or-channel convention and the tests pin it. One input is returned untouched — there is nothing to combine, and wrapping it would only add a goroutine.

The recursive `OrRecursive` merges pairwise in a balanced tree. It splits the inputs in half, builds an or-channel for each half, then ors those two halves. The base cases are zero (already-closed channel), one (return it), and two (a single goroutine that selects on both inputs and closes its output with `defer`). A balanced tree with N leaves has N-1 internal nodes, and each internal node is one two-input merge spawning exactly one goroutine, so `OrRecursive` uses N-1 goroutines — the same O(N) order as the flat form, not O(log N). What the recursion changes is real but narrower than the folklore: the construction call stack is O(log N) deep, and every spawned goroutine selects over exactly two channels and closes a private output it alone owns, so no shared `sync.Once` is needed. Reach for the recursive form when you want that single-owner-per-output property; reach for the flat form, the simpler default, the rest of the time.

Create `orchann.go`:

```go
package orchann

import "sync"

// Or combines the input channels into one. The returned channel closes as soon
// as any input closes. It spawns one goroutine per non-nil input (the flat
// fan-in form). Nil inputs are skipped because a receive on a nil channel
// blocks forever. Calling Or with no inputs returns an already-closed channel.
func Or(cs ...<-chan struct{}) <-chan struct{} {
	switch len(cs) {
	case 0:
		out := make(chan struct{})
		close(out)
		return out
	case 1:
		return cs[0]
	}

	out := make(chan struct{})
	var once sync.Once
	closeOut := func() { once.Do(func() { close(out) }) }

	for _, c := range cs {
		if c == nil {
			continue
		}
		go func(c <-chan struct{}) {
			select {
			case <-c:
				closeOut()
			case <-out:
				// Another input already won the race; exit cleanly.
			}
		}(c)
	}
	return out
}

// OrRecursive combines the input channels by merging them pairwise in a
// balanced tree. It uses N-1 goroutines for N inputs (the same O(N) order as
// Or), but the construction stack is O(log N) deep and each goroutine owns the
// output it closes, so no shared sync.Once is needed.
func OrRecursive(cs ...<-chan struct{}) <-chan struct{} {
	switch len(cs) {
	case 0:
		out := make(chan struct{})
		close(out)
		return out
	case 1:
		return cs[0]
	case 2:
		out := make(chan struct{})
		go func() {
			defer close(out)
			select {
			case <-cs[0]:
			case <-cs[1]:
			}
		}()
		return out
	default:
		mid := len(cs) / 2
		return OrRecursive(OrRecursive(cs[:mid]...), OrRecursive(cs[mid:]...))
	}
}
```

In `Or`, the close helper is the only place `close(out)` appears, and it runs inside `once.Do`, so the panic from a double close is structurally impossible. Each goroutine takes its channel as a parameter rather than capturing a loop variable, which keeps the binding explicit even though Go 1.22 and later already scope the loop variable per iteration. In `OrRecursive`, the two-input base case is the only place a goroutine is created; every larger call decomposes into those base cases, so each merge has exactly one closer and `defer close(out)` is safe without a guard.

### The runnable demo

The demo makes the "first wins" behavior concrete. It launches three goroutines that close three channels at 30, 60, and 90 milliseconds, waits on `Or` of all three, and reports how long it blocked. The combined channel closes at roughly the first deadline, not the last.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/or-channel-core"
)

func main() {
	a := make(chan struct{})
	b := make(chan struct{})
	c := make(chan struct{})

	go func() { time.Sleep(30 * time.Millisecond); close(a) }()
	go func() { time.Sleep(60 * time.Millisecond); close(b) }()
	go func() { time.Sleep(90 * time.Millisecond); close(c) }()

	start := time.Now()
	<-orchann.Or(a, b, c)
	elapsed := time.Since(start)

	// The combined channel fires at the first closer (~30ms), not the last.
	fmt.Printf("first signal fired after ~%dms\n", elapsed.Round(10*time.Millisecond).Milliseconds())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first signal fired after ~30ms
```

### Tests

The suite pins every clause of the contract. `TestOrZeroInputsClosesImmediately` proves the zero-input convention. `TestOrSingleInputPassthrough` proves one input is returned untouched and still propagates its close. `TestOrFirstInputFires` and `TestOrIgnoresNil` cover the core behavior past nils. `TestOrCloseOnceUnderRace` closes 32 inputs in parallel and asserts the output closes exactly once with no panic — the test the `sync.Once` exists for. `TestOrRecursiveParity` holds `OrRecursive` to the same first-fires behavior. `TestOrHundredChannels` (the former "your turn") builds 100 channels, closes the 50th, and asserts the combined channel closes within the window.

Create `orchann_test.go`:

```go
package orchann

import (
	"sync"
	"testing"
	"time"
)

func TestOrZeroInputsClosesImmediately(t *testing.T) {
	t.Parallel()

	select {
	case <-Or():
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Or() with no inputs did not close")
	}
}

func TestOrSingleInputPassthrough(t *testing.T) {
	t.Parallel()

	ch := make(chan struct{})
	out := Or(ch)

	select {
	case <-out:
		t.Fatal("Or(ch) closed before its input did")
	default:
	}

	close(ch)
	select {
	case <-out:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Or(ch) did not propagate close")
	}
}

func TestOrFirstInputFires(t *testing.T) {
	t.Parallel()

	fast := make(chan struct{})
	slow := make(chan struct{})
	out := Or(fast, slow)

	close(fast)
	select {
	case <-out:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Or did not close when the first input fired")
	}

	close(slow) // closing the loser afterwards must not panic.
}

func TestOrIgnoresNil(t *testing.T) {
	t.Parallel()

	ch := make(chan struct{})
	out := Or(nil, ch, nil)

	close(ch)
	select {
	case <-out:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Or did not propagate close past nil inputs")
	}
}

func TestOrCloseOnceUnderRace(t *testing.T) {
	t.Parallel()

	const n = 32
	chs := make([]chan struct{}, n)
	inputs := make([]<-chan struct{}, n)
	for i := range chs {
		chs[i] = make(chan struct{})
		inputs[i] = chs[i]
	}

	out := Or(inputs...)

	var wg sync.WaitGroup
	for _, c := range chs {
		wg.Add(1)
		go func(c chan struct{}) {
			defer wg.Done()
			close(c)
		}(c)
	}
	wg.Wait()

	select {
	case <-out:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Or did not close")
	}
}

func TestOrRecursiveParity(t *testing.T) {
	t.Parallel()

	a := make(chan struct{})
	b := make(chan struct{})
	c := make(chan struct{})
	out := OrRecursive(a, b, c)

	close(b)
	select {
	case <-out:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("OrRecursive did not close when the middle input fired")
	}
	close(a)
	close(c)
}

func TestOrHundredChannels(t *testing.T) {
	t.Parallel()

	const n = 100
	chs := make([]chan struct{}, n)
	inputs := make([]<-chan struct{}, n)
	for i := range chs {
		chs[i] = make(chan struct{})
		inputs[i] = chs[i]
	}

	out := Or(inputs...)
	close(chs[50])

	select {
	case <-out:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Or(100 channels) did not close when one input fired")
	}
}
```

## Review

The pattern is correct when the output closes exactly once and no goroutine leaks. In `Or`, both guarantees come from the same two lines: `close(out)` lives only inside `once.Do`, so racing closers cannot double-close, and every goroutine carries a `<-out` arm, so the loser of the race always has an exit. Drop the `sync.Once` and `TestOrCloseOnceUnderRace` panics; drop the `<-out` arm and the losing goroutines leak on inputs that never fire. In `OrRecursive`, the single-closer property is structural — every output is closed by exactly one goroutine through `defer close(out)` — which is why it needs no `sync.Once` at all.

The common mistakes are three. Closing the output from every goroutine instead of through `sync.Once` is the double-close panic. Forgetting that a nil input blocks forever leaks a goroutine, which is why `Or` skips nils before spawning and `TestOrIgnoresNil` feeds it nils on both sides of a live channel. And describing `OrRecursive` as O(log N) goroutines is simply false: it is N-1 goroutines, O(N), the same order as `Or`; only the construction stack is logarithmic. Confirm all of it with `go test -race ./...`: the race detector is what turns the close-once contract from a claim into a checked fact.

## Resources

- [Advanced Go Concurrency Patterns](https://go.dev/blog/advanced-go-concurrency-patterns) — the Go blog talk that introduces the `or` combinator and the select-on-done idiom.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — the standard-library primitive that makes the single close idempotent under racing goroutines.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — how closed channels broadcast cancellation to many goroutines at once.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-combine-cancellation-sources.md](02-combine-cancellation-sources.md)
</content>
