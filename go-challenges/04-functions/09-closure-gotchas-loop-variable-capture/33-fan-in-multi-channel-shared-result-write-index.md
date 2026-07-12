# Exercise 33: Multi-Channel Fan-In: Goroutines Writing Shared Result Slice Using Loop Index

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A fan-in stage drains several input channels concurrently, one goroutine per
channel, each meant to write the count of values it read into its own slot
in a shared `results` slice. The trap: computing that slot from a single
shared `idx` variable declared outside the loop instead of a parameter, so
every goroutine ends up targeting the SAME slot — whichever channel the
dispatch loop last got to — and every other channel's count is silently
lost.

## What you'll build

```text
fanin/                       independent module: example.com/fanin
  go.mod                     go 1.24
  fanin.go                    FanIn, FanInBuggy
  cmd/
    demo/
      main.go                runnable demo: fan in 3 channels, print per-channel counts
  fanin_test.go                table test: disjoint slots vs collapsed slot; edge case
```

- Files: `fanin.go`, `cmd/demo/main.go`, `fanin_test.go`.
- Implement: `FanIn` spawning one goroutine per channel that writes its own count into `results[i]` passed as a parameter; `FanInBuggy` spawning goroutines that all read a single shared `idx` variable instead.
- Test: fan in several channels of different lengths and assert `FanIn`'s counts land in the correct slots while `FanInBuggy`'s collapse into the last slot; `-race` clean.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/33-fan-in-multi-channel-shared-result-write-index/cmd/demo
cd go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/33-fan-in-multi-channel-shared-result-write-index
go mod edit -go=1.24
```

### Why the slot index must be a parameter, not a shared variable

Each goroutine must know which slot in `results` is its own. Distinct
indices are distinct memory, so disjoint-slot writes need no lock at all —
`FanIn` passes `i` as an argument to the goroutine closure, so `results[i]`
is always that goroutine's own slot regardless of scheduling or how long any
particular channel takes to drain.

`FanInBuggy` recreates the classic mistake deliberately: `idx` is declared
once outside the loop, and every goroutine reads it instead of its own
index, after fully draining its own channel first (which can take a while
and vary per channel). A barrier makes the worst case deterministic instead
of just possible: every goroutine waits on `release` until the dispatch loop
has finished advancing `idx` to the LAST channel's index, then they all read
it. The write to `results[idx]` is mutex-protected, so the concurrent writes
themselves are not a data race — the bug is entirely in *which* slot every
goroutine targets. Because addition is commutative, every channel's count
ends up added into the SAME last slot, and every other slot is silently left
at zero.

Create `fanin.go`:

```go
package fanin

import "sync"

// FanIn drains every channel to completion concurrently and writes the
// count of values read from channels[i] into results[i]. Each goroutine
// receives its own index as a parameter, so disjoint slots need no lock at
// all -- results[i] is always that goroutine's own slot regardless of
// scheduling.
func FanIn(channels []<-chan int) []int {
	results := make([]int, len(channels))
	var wg sync.WaitGroup
	for i, ch := range channels {
		wg.Add(1)
		go func(i int, ch <-chan int) {
			defer wg.Done()
			n := 0
			for range ch {
				n++
			}
			results[i] = n
		}(i, ch)
	}
	wg.Wait()
	return results
}

// FanInBuggy drains every channel concurrently too, but every goroutine
// writes its count into a slot read from a SINGLE shared `idx` variable
// declared outside the loop instead of its own index. A goroutine drains its
// own channel (which can take a while) before it ever looks at idx, so this
// exercise makes the worst case deterministic with a barrier: every
// goroutine waits on `release` until the dispatch loop has finished
// advancing idx to the LAST channel's index, then they all read it and add
// their count into results[idx]. The write is mutex-protected, so this is a
// logic bug, not a data race: every channel's count ends up added into the
// SAME last slot, and every other slot is silently left at zero.
func FanInBuggy(channels []<-chan int) []int {
	results := make([]int, len(channels))
	var idx int // BUG: one shared index for every goroutine instead of its own
	var mu sync.Mutex
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i, ch := range channels {
		idx = i
		wg.Add(1)
		go func(ch <-chan int) {
			defer wg.Done()
			n := 0
			for range ch {
				n++
			}
			<-release // wait for dispatch to finish advancing idx
			mu.Lock()
			results[idx] += n // BUG: idx now holds the LAST channel's index for every goroutine
			mu.Unlock()
		}(ch)
	}
	close(release) // only now do goroutines read idx; every read sees the final value
	wg.Wait()
	return results
}
```

### The runnable demo

The demo fans in three channels holding 2, 5, and 3 values respectively.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fanin"
)

// makeChannels returns one closed, pre-filled channel per count in counts.
func makeChannels(counts []int) []<-chan int {
	out := make([]<-chan int, len(counts))
	for i, n := range counts {
		ch := make(chan int, n)
		for v := 0; v < n; v++ {
			ch <- v
		}
		close(ch)
		out[i] = ch
	}
	return out
}

func main() {
	counts := []int{2, 5, 3}

	correct := fanin.FanIn(makeChannels(counts))
	fmt.Println("correct per-channel counts:", correct)

	buggy := fanin.FanInBuggy(makeChannels(counts))
	fmt.Println("buggy   per-channel counts:", buggy)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
correct per-channel counts: [2 5 3]
buggy   per-channel counts: [0 0 10]
```

### Tests

`TestFanIn` is a table test asserting `FanIn` writes each channel's count
into its own slot while `FanInBuggy` collapses every count into the last
slot (the sum of all three, since addition is commutative).
`TestFanInSingleChannelEdgeCase` covers the boundary where there is no other
slot to collapse into.

Create `fanin_test.go`:

```go
package fanin

import "testing"

func makeChannels(counts []int) []<-chan int {
	out := make([]<-chan int, len(counts))
	for i, n := range counts {
		ch := make(chan int, n)
		for v := 0; v < n; v++ {
			ch <- v
		}
		close(ch)
		out[i] = ch
	}
	return out
}

func TestFanIn(t *testing.T) {
	counts := []int{2, 5, 3}
	var total int
	for _, n := range counts {
		total += n
	}
	lastIdx := len(counts) - 1

	tests := []struct {
		name  string
		fanIn func([]<-chan int) []int
		want  []int
	}{
		{
			name:  "fixed: each channel's count lands in its own slot",
			fanIn: FanIn,
			want:  []int{2, 5, 3},
		},
		{
			name:  "buggy: every channel's count collapses into the last slot",
			fanIn: FanInBuggy,
			want:  []int{0, 0, total},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.fanIn(makeChannels(counts))
			if len(got) != len(tt.want) {
				t.Fatalf("results = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					if i == lastIdx {
						t.Fatalf("results[%d] = %d, want %d (sum of every channel's count)", i, got[i], tt.want[i])
					}
					t.Fatalf("results[%d] = %d, want %d", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestFanInSingleChannelEdgeCase(t *testing.T) {
	t.Parallel()

	counts := []int{7}
	correct := FanIn(makeChannels(counts))
	buggy := FanInBuggy(makeChannels(counts))

	if correct[0] != 7 {
		t.Fatalf("correct[0] = %d, want 7", correct[0])
	}
	if buggy[0] != 7 {
		t.Fatalf("buggy[0] = %d, want 7 (single channel has no other slot to collapse into)", buggy[0])
	}
}
```

## Review

Fan-in is correct when every channel's count lands in its own slot, no
matter how many goroutines drain concurrently or how long any one channel
takes. The mechanism to keep straight is that a goroutine's parameters are
copied at the `go` statement, so passing `i` freezes that goroutine's own
value regardless of what the dispatch loop does afterward; closing over a
shared variable instead means every goroutine reads whatever that variable
holds when it happens to check it. The barrier in `FanInBuggy` does not
change the bug, it only pins the outcome so the test is deterministic
instead of a flake. Run `go test -race`; the disjoint-slot writes in `FanIn`
and the mutex-guarded writes in `FanInBuggy` must both be clean.

## Resources

- [Go spec: Go statements](https://go.dev/ref/spec#Go_statements) — function arguments are evaluated when the `go` statement executes, not when the goroutine runs.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — coordinating completion of a fan-out of worker goroutines.
- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — why the slot index is safe per-iteration but still best passed explicitly.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-dns-cache-entry-pointer-capture-mutation-race.md](32-dns-cache-entry-pointer-capture-mutation-race.md) | Next: [34-shutdown-cleanup-stack-cancelctx-capture-timing.md](34-shutdown-cleanup-stack-cancelctx-capture-timing.md)
