# Exercise 3: A Runnable Demo That Interleaves a Fast and Slow Producer

A library that only ever runs inside its own `*_test.go` has not proven it
composes. This module ships an executable that constructs a fast producer and a
slow producer at different cadences, feeds both into a fan-in merge, and prints the
interleaved stream — a runnable proof that the merge works outside the test package
and that `select`'s fairness genuinely interleaves the two rates instead of
draining one then the other.

This module is fully self-contained: its own `go mod init`, its own copy of the
merge, its own `cmd/demo`, and a test so the binary is not left unverified. It
imports no other exercise.

## What you'll build

```text
feeddemo/                       module example.com/feeddemo
  go.mod                        go 1.26
  feeddemo.go                   FanIn merge + Run(w io.Writer) that drives two producers
  cmd/
    demo/
      main.go                   package main: Run(os.Stdout)
  feeddemo_test.go              capture Run's output; assert every token once, any order
```

Files: `feeddemo.go`, `cmd/demo/main.go`, `feeddemo_test.go`.
Implement: `FanIn` (bundled here so the module stands alone) and `Run(w io.Writer)` driving a fast + a slow producer through it.
Test: run `Run` into a buffer and assert every expected token appears exactly once regardless of order.
Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`

## Why the work lives in Run(w io.Writer), not in main

The naive way to write a demo is to put everything in `main()` and eyeball the
output. That leaves the binary untested: nothing fails when the merge regresses.
The fix is to put the actual work in an exported `Run(w io.Writer)` and make
`main` a one-liner, `Run(os.Stdout)`. Now the same code path the binary exercises
can be driven from a test with a `bytes.Buffer` and asserted, so the executable is
covered.

`Run` starts the fast and slow producers *before* the consumer ranges over the
merged channel — each producer goroutine begins its own sends inside `Run`, and
the fan-in wiring returns the output channel already backed by live producers.
This is the fix for the "spawn the producer after the consumer selects" race: the
producers are launched in the same function that builds and consumes the merged
stream, so there is no window where the consumer selects on channels no one is
feeding yet.

The exact interleaving of `f*` and `s*` lines is nondeterministic — it depends on
the scheduler and on `select`'s pseudo-random arbitration inside the merge — so the
test cannot assert an order. What it *can* assert, and does, is a multiset
invariant: every token the two producers emit shows up in the merged output
exactly once, and nothing else does. That is the honest contract for a
nondeterministic stream.

Create `feeddemo.go`:

```go
package feeddemo

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// FanIn is bundled here so this module stands alone. It merges N sources into
// one channel closed once, after every source drains.
func FanIn[T any](sources ...<-chan T) <-chan T {
	out := make(chan T)
	var wg sync.WaitGroup
	for _, src := range sources {
		wg.Go(func() {
			for v := range src {
				out <- v
			}
		})
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

// producer emits each token with the given cadence between sends, then closes.
func producer(tokens []string, cadence time.Duration) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		for _, tok := range tokens {
			ch <- tok
			time.Sleep(cadence)
		}
	}()
	return ch
}

// Run drives a fast producer and a slow producer through FanIn and writes each
// merged token, one per line, to w. It returns after the merged stream closes.
func Run(w io.Writer) {
	fast := producer([]string{"f1", "f2", "f3"}, 5*time.Millisecond)
	slow := producer([]string{"s1", "s2", "s3"}, 15*time.Millisecond)

	merged := FanIn(fast, slow)
	for tok := range merged {
		fmt.Fprintln(w, tok)
	}
}
```

## The runnable demo

`main` just points `Run` at standard output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/feeddemo"
)

func main() {
	feeddemo.Run(os.Stdout)
}
```

Run with `go run ./cmd/demo`. The six tokens interleave; one *possible* run:

```
f1
s1
f2
f3
s2
s3
```

The order varies run to run — only the set of six tokens is guaranteed.

## Tests

The test drives the exact code path the binary uses. It runs `Run` into a
`bytes.Buffer`, splits the captured lines, and asserts the multiset equals
`{f1, f2, f3, s1, s2, s3}` — every token exactly once, nothing extra. Because it
asserts a set and not a sequence, it is immune to the nondeterministic
interleaving while still catching a dropped, duplicated, or invented token.

Create `feeddemo_test.go`:

```go
package feeddemo

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunEmitsEveryTokenExactlyOnce(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	Run(&buf)

	got := map[string]int{}
	for _, line := range strings.Fields(buf.String()) {
		got[line]++
	}

	want := []string{"f1", "f2", "f3", "s1", "s2", "s3"}
	if len(got) != len(want) {
		t.Fatalf("distinct tokens = %d, want %d (%v)", len(got), len(want), got)
	}
	for _, tok := range want {
		if got[tok] != 1 {
			t.Fatalf("token %q appeared %d times, want 1 (full output: %v)", tok, got[tok], got)
		}
	}
}
```

## Review

The demo is doing its job when `go run ./cmd/demo` prints six interleaved lines and
the test passes deterministically despite the nondeterministic order. The design
lesson is structural: work belongs in an `io.Writer`-taking function so the binary
is testable, and producers must be launched in the same function that consumes the
merge so there is no scheduling window where the consumer selects on unfed
channels. Assert the multiset, never the sequence — encoding the observed order
into the test would make it flake on the next scheduler decision. `-race` confirms
the two producers plus the merge closer are synchronized.

## Resources

- [io.Writer](https://pkg.go.dev/io#Writer) — the interface that makes `Run` testable without capturing `os.Stdout`.
- [testing package](https://pkg.go.dev/testing) — running an exported entry point from a table-free test.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — producer/consumer wiring done right.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-fan-in-merge.md](02-fan-in-merge.md) | Next: [04-done-channel-worker.md](04-done-channel-worker.md)
