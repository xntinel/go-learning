# Exercise 4: Monotonic ID / Sequence Generator

Request IDs, message sequence numbers, optimistic-lock version stamps — a backend
constantly needs a source of strictly increasing `uint64`s that never repeats
across goroutines. This exercise builds that generator on a single
`atomic.Uint64.Add(1)`, and pins down the one API detail that makes it correct:
`Add` returns the NEW value.

This module is fully self-contained.

## What you'll build

```text
seqgen/                    independent module: example.com/seqgen
  go.mod
  seq.go                   type Generator; Next (Add), Peek (Load)
  cmd/
    demo/
      main.go              hands out a few IDs
  seq_test.go              N*M distinct-and-gapless test, deterministic test, Example
```

- Files: `seq.go`, `cmd/demo/main.go`, `seq_test.go`.
- Implement: a `Generator` over `atomic.Uint64`; `Next` returns a strictly increasing ID via `Add(1)`; `Peek` reports the last issued ID.
- Test: N goroutines each pull M IDs; assert exactly N*M distinct IDs with no duplicates and no gaps up to the max.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/seqgen/cmd/demo
cd ~/go-exercises/seqgen
go mod init example.com/seqgen
```

### Why Add(1), and the double-generate bug it avoids

The entire generator is one line: `return g.n.Add(1)`. `atomic.Uint64.Add` performs
the increment and returns the *new* value as a single atomic step, so two goroutines
calling `Next` concurrently are guaranteed to receive two different numbers — one
gets `k`, the other gets `k+1`, in some order, with no overlap and nothing lost. The
first ID is `1` (the counter starts at zero and `Add(1)` returns the post-increment
value), and the IDs are strictly increasing and gapless.

The tempting-but-wrong alternative is `id := g.n.Load(); g.n.Add(1); return id` —
"read the current value, then bump it". Under concurrency two goroutines can `Load`
the same value before either `Add`s, and both return the same ID: a duplicate. This
is the double-generate bug, and it is why you must use `Add`'s return value rather
than a separate load. `Add` is a single instruction precisely so the read and the
increment cannot be split apart.

Note the return-value discipline from the concepts: `Add` returns the NEW value,
`Swap` returns the OLD value, `CompareAndSwap` returns a bool. If you wanted a
zero-based sequence you would compute `g.n.Add(1) - 1`; here we want one-based, so we
return the result directly. `Peek` is a plain `Load` — it reports the highest ID
issued so far without consuming one, useful for metrics or checkpointing.

Create `seq.go`:

```go
package seqgen

import "sync/atomic"

// Generator hands out strictly increasing uint64 IDs for request-ids, message
// sequence numbers, or optimistic-lock versions. Safe for concurrent use.
type Generator struct {
	n atomic.Uint64
}

// Next returns the next ID. IDs are strictly increasing, gapless, and unique
// across concurrent callers. The first ID returned is 1.
func (g *Generator) Next() uint64 {
	return g.n.Add(1)
}

// Peek reports the highest ID issued so far without consuming a new one.
func (g *Generator) Peek() uint64 {
	return g.n.Load()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/seqgen"
)

func main() {
	var g seqgen.Generator
	for range 3 {
		fmt.Println("id:", g.Next())
	}
	fmt.Println("peek:", g.Peek())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id: 1
id: 2
id: 3
peek: 3
```

### Tests

`TestNoDuplicatesNoGaps` is the proof: `N` goroutines each pull `M` IDs into a
shared, mutex-guarded set. Afterward there must be exactly `N*M` distinct IDs, and
every integer from `1` to `N*M` must be present — no duplicate (which would prove a
lost increment) and no gap (which would prove a skipped one). Under `-race` this
demonstrates `Add` is a genuine atomic read-modify-write.

Create `seq_test.go`:

```go
package seqgen

import (
	"fmt"
	"sync"
	"testing"
)

func TestNoDuplicatesNoGaps(t *testing.T) {
	t.Parallel()

	var g Generator
	const goroutines = 50
	const perGoroutine = 200
	total := goroutines * perGoroutine

	var mu sync.Mutex
	seen := make(map[uint64]bool, total)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			local := make([]uint64, 0, perGoroutine)
			for range perGoroutine {
				local = append(local, g.Next())
			}
			mu.Lock()
			for _, id := range local {
				seen[id] = true
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("distinct IDs = %d, want %d (duplicate issued)", len(seen), total)
	}
	for i := uint64(1); i <= uint64(total); i++ {
		if !seen[i] {
			t.Fatalf("missing ID %d (gap in sequence)", i)
		}
	}
	if got := g.Peek(); got != uint64(total) {
		t.Fatalf("Peek() = %d, want %d", got, total)
	}
}

func TestStrictlyIncreasing(t *testing.T) {
	t.Parallel()

	var g Generator
	prev := g.Next()
	for range 1000 {
		cur := g.Next()
		if cur <= prev {
			t.Fatalf("non-increasing: %d after %d", cur, prev)
		}
		prev = cur
	}
}

func ExampleGenerator() {
	var g Generator
	fmt.Println(g.Next(), g.Next(), g.Next())
	// Output: 1 2 3
}
```

## Review

The generator is correct when the multiset of issued IDs is exactly `{1, ..., N*M}`
with no repeats — that is what `Add`'s atomic return guarantees and what
`TestNoDuplicatesNoGaps` verifies under `-race`. The one mistake that breaks it is
splitting the read from the increment (`Load` then `Add`), which lets two callers
issue the same ID; always return `Add`'s result. Remember the return-value contract:
`Add` gives you the post-increment value, so the first ID is `1`, not `0`.

## Resources

- [`atomic.Uint64.Add`](https://pkg.go.dev/sync/atomic#Uint64.Add) — returns the new value; the whole generator.
- [Go 1.19 release notes: atomic types](https://go.dev/doc/go1.19#atomic_types) — the typed wrappers.
- [The Go Memory Model](https://go.dev/ref/mem) — why the increment cannot be lost.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-cas-state-machine.md](03-cas-state-machine.md) | Next: [05-peak-gauge-highwater-cas.md](05-peak-gauge-highwater-cas.md)
