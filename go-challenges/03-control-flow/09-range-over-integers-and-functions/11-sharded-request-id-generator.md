# Exercise 11: Sharded Request ID Generator — `for i := range n` Driving a Round-Robin Allocator

**Nivel: Intermedio** — validacion rapida (un test corto).

A job queue with N worker shards needs request IDs that are both globally
ordered-enough for debugging and cheap to assign without coordination: round-robin
the sequence number across shards as `s<shard>-<seq>`. This exercise builds that
allocator two ways — a classic counted loop and a lazy `iter.Seq[string]` — so you
see `for i := range n` as the plain engine underneath a lazy stream.

## What you'll build

```text
shardid/                 independent module: example.com/shardid
  go.mod                  module example.com/shardid
  shardid.go              ShardOf, Generate, Seq
  shardid_test.go         round-robin correctness + lazy-stop test
```

Implement: `ShardOf(i, numShards int) int`, `Generate(numShards, n int) []string`, and `Seq(numShards, n int) iter.Seq[string]` yielding the same IDs lazily.
Test: round-robin assignment for 3 shards over 7 IDs; `Seq` matches `Generate` value for value; `Seq` stops after a consumer break without generating the rest; a single shard is purely sequential.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shardid
cd ~/go-exercises/shardid
go mod init example.com/shardid
go mod edit -go=1.24
```

Create `shardid.go`:

```go
package shardid

import (
	"fmt"
	"iter"
)

// ShardOf returns the shard index that the i-th (0-based) generated ID lands
// on when IDs are assigned round-robin across numShards shards.
func ShardOf(i, numShards int) int {
	return i % numShards
}

// Generate returns the first n IDs assigned round-robin across numShards
// shards, formatted as "s<shard>-<seq>" where seq is the per-shard sequence
// number, zero-padded to width 6. This is the classic counted-loop version.
func Generate(numShards, n int) []string {
	ids := make([]string, 0, n)
	counters := make([]int, numShards)
	for i := range n {
		shard := ShardOf(i, numShards)
		ids = append(ids, fmt.Sprintf("s%d-%06d", shard, counters[shard]))
		counters[shard]++
	}
	return ids
}

// Seq is the lazy, range-over-func equivalent of Generate: it yields the same
// IDs one at a time instead of building the whole slice up front, so a caller
// that stops early never pays for the unconsumed IDs.
func Seq(numShards, n int) iter.Seq[string] {
	return func(yield func(string) bool) {
		counters := make([]int, numShards)
		for i := range n {
			shard := ShardOf(i, numShards)
			id := fmt.Sprintf("s%d-%06d", shard, counters[shard])
			counters[shard]++
			if !yield(id) {
				return
			}
		}
	}
}
```

Create `shardid_test.go`:

```go
package shardid

import "testing"

func TestGenerateRoundRobin(t *testing.T) {
	t.Parallel()

	got := Generate(3, 7)
	want := []string{
		"s0-000000", "s1-000000", "s2-000000",
		"s0-000001", "s1-000001", "s2-000001",
		"s0-000002",
	}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSeqStopsEarly(t *testing.T) {
	t.Parallel()

	count := 0
	for range Seq(4, 100) {
		count++
		if count == 5 {
			break
		}
	}
	if count != 5 {
		t.Fatalf("count = %d, want 5", count)
	}
}
```

## Verify

```bash
go test -count=1 ./...
```

## Review

`Generate` is the ordinary counted loop: `for i := range n` drives shard
assignment and a per-shard counter, and the whole result is materialized before
the caller sees any of it. `Seq` is the same allocation logic wearing an
`iter.Seq[string]` shape — same loop, same per-shard state, but each ID is handed
to `yield` the instant it exists, and `if !yield(id) { return }` means a consumer
that only wants the first five IDs never pays for generating the other ninety-five.
That is the whole lesson of range-over-int versus range-over-func: they solve the
same counting problem, but only one of them lets the consumer control how much
work actually happens.

## Resources

- [Go spec: `for` range clause (integers)](https://go.dev/ref/spec#For_range)
- [`iter` package documentation](https://pkg.go.dev/iter)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-error-carrying-seq2.md](10-error-carrying-seq2.md) | Next: [12-nth-event-sampling-combinator.md](12-nth-event-sampling-combinator.md)
