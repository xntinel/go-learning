# Exercise 9: Fuzz A Token-Bucket Rate Limiter's State Invariants

A rate limiter is stateful: its behavior depends on the whole history of refills
and takes, not on a single input. Example tests exercise a handful of sequences;
the interleaving that overflows the counter or drives it negative is the one you
did not write. This module builds a pure token bucket and fuzzes it with a
model-based approach — the fuzzed bytes become a random *sequence* of operations,
and after every step the state invariant `0 <= tokens <= capacity` must hold.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
ratelimit/                 independent module: example.com/ratelimit
  go.mod                   module path
  bucket.go                Bucket; NewBucket, Advance, TryTake, Tokens, Capacity
  cmd/
    demo/
      main.go              drain the bucket, refill over time, take again
  bucket_test.go           TestBucketSequence, TestOverflowClamp, FuzzBucketInvariant, Example
```

Files: `bucket.go`, `cmd/demo/main.go`, `bucket_test.go`.
Implement: a pure `Bucket` with `Advance(elapsedMillis int64)` and
`TryTake(n uint64) bool`, no real clock.
Test: deterministic sequence and overflow-clamp tests; `FuzzBucketInvariant`
replaying a fuzzed op stream and asserting the invariant after each step.
Verify: `go test -race ./...`, then `go test -fuzz=FuzzBucketInvariant
-fuzztime=2s`.

Set up the module:

```bash
mkdir -p ~/go-exercises/ratelimit/cmd/demo
cd ~/go-exercises/ratelimit
go mod init example.com/ratelimit
```

### A pure limiter, and model-based fuzzing

The bucket holds `tokens` up to `capacity`, refilling at `rate` tokens per
millisecond. Making it *pure* — `Advance(elapsedMillis)` takes the elapsed time as
an argument rather than reading a real clock — is what makes it fuzzable: the fuzz
body must be deterministic, and a limiter that called `time.Now()` internally
could not be replayed. In production you would drive `Advance` from a real
monotonic clock; here time is just a number the test controls.

Two arithmetic hazards make this exactly the kind of code fuzzing is built for.
`Advance` adds `elapsedMillis * rate` tokens, and both a large elapsed and a large
rate can overflow `uint64` before the clamp to `capacity` runs — so the clamp must
be computed *without* performing the overflowing multiply. `TryTake` subtracts,
and subtracting more than is present would underflow a `uint64` into a gigantic
number — so it must check `n > tokens` first. Both are one-line bugs that a fixed
test suite sails past and a fuzzer finds in milliseconds.

Model-based fuzzing turns a flat `[]byte` into a *sequence of operations*. The
body reads the bytes as a stream of `binary.Uvarint` values; each value's low bit
picks the operation (advance or take) and the remaining bits are its magnitude. It
replays that sequence against the bucket and, after every single operation,
asserts the state invariant `0 <= tokens <= capacity`. Because `tokens` is a
`uint64` the lower bound is automatic; the upper bound is the real assertion, and
it is checked after *each* step so a violating interleaving is caught at the exact
operation that broke it. This is how you find the overflow that only happens after
a specific run of takes followed by a huge advance.

Create `bucket.go`:

```go
package ratelimit

// Bucket is a pure token-bucket rate limiter. It holds no clock; callers drive
// time forward with Advance. The invariant 0 <= tokens <= capacity holds after
// every operation.
type Bucket struct {
	capacity uint64
	rate     uint64 // tokens added per millisecond
	tokens   uint64
}

// NewBucket returns a full bucket of the given capacity refilling at ratePerMilli
// tokens per millisecond.
func NewBucket(capacity, ratePerMilli uint64) *Bucket {
	return &Bucket{capacity: capacity, rate: ratePerMilli, tokens: capacity}
}

// Tokens reports the current token count.
func (b *Bucket) Tokens() uint64 { return b.tokens }

// Capacity reports the maximum token count.
func (b *Bucket) Capacity() uint64 { return b.capacity }

// Advance refills the bucket for elapsedMillis of elapsed time, clamped to
// capacity. It computes the clamp without an overflowing multiply.
func (b *Bucket) Advance(elapsedMillis int64) {
	if elapsedMillis <= 0 || b.rate == 0 {
		return
	}
	room := b.capacity - b.tokens
	e := uint64(elapsedMillis)
	if e > room/b.rate { // e*rate would overshoot room; clamp instead of multiply
		b.tokens = b.capacity
		return
	}
	b.tokens += e * b.rate
}

// TryTake removes n tokens if at least n are available, reporting success. It
// checks availability first so the uint64 subtraction never underflows.
func (b *Bucket) TryTake(n uint64) bool {
	if n > b.tokens {
		return false
	}
	b.tokens -= n
	return true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ratelimit"
)

func main() {
	b := ratelimit.NewBucket(10, 2) // 10 tokens, refills 2/ms
	fmt.Printf("start: %d\n", b.Tokens())

	fmt.Printf("take 10: %v (now %d)\n", b.TryTake(10), b.Tokens())
	fmt.Printf("take 1 (empty): %v (now %d)\n", b.TryTake(1), b.Tokens())

	b.Advance(3) // 3ms * 2 = 6 tokens
	fmt.Printf("after 3ms: %d\n", b.Tokens())

	b.Advance(1000) // way more than capacity: clamps
	fmt.Printf("after long idle: %d\n", b.Tokens())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
start: 10
take 10: true (now 0)
take 1 (empty): false (now 0)
after 3ms: 6
after long idle: 10
```

### Tests

Create `bucket_test.go`:

```go
package ratelimit

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"
)

func TestBucketSequence(t *testing.T) {
	t.Parallel()
	b := NewBucket(10, 1)
	if !b.TryTake(10) {
		t.Fatal("first take of full bucket failed")
	}
	if b.TryTake(1) {
		t.Fatal("take from empty bucket succeeded")
	}
	b.Advance(3) // +3 tokens
	if got := b.Tokens(); got != 3 {
		t.Fatalf("after Advance(3), tokens = %d, want 3", got)
	}
	if !b.TryTake(3) {
		t.Fatal("take of exactly available tokens failed")
	}
}

func TestOverflowClamp(t *testing.T) {
	t.Parallel()
	b := NewBucket(10, math.MaxUint64) // absurd rate
	b.TryTake(10)
	b.Advance(math.MaxInt64) // elapsed*rate overflows uint64 if multiplied
	if got := b.Tokens(); got != 10 {
		t.Fatalf("after overflow Advance, tokens = %d, want 10 (clamped)", got)
	}
}

func FuzzBucketInvariant(f *testing.F) {
	seeds := [][]byte{
		{},
		{0x02, 0x03},                   // advance 1, take 1
		{0xff, 0x01, 0x00},             // a big take, an advance
		{0x14, 0x14, 0x14, 0x15, 0x15}, // bursts of advances then takes
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, ops []byte) {
		b := NewBucket(1000, 5)
		data := ops
		for len(data) > 0 {
			v, n := binary.Uvarint(data)
			if n <= 0 {
				break // incomplete or overflowing varint ends the stream
			}
			data = data[n:]
			if v&1 == 0 {
				b.Advance(int64(v >> 1))
			} else {
				b.TryTake(v >> 1)
			}
			if b.Tokens() > b.Capacity() {
				t.Fatalf("invariant broken: tokens %d > capacity %d", b.Tokens(), b.Capacity())
			}
		}
	})
}

func Example() {
	b := NewBucket(5, 1)
	fmt.Println(b.TryTake(3), b.Tokens())
	// Output: true 2
}
```

## Review

The bucket is correct when `0 <= tokens <= capacity` holds after every operation
for every op sequence the fuzzer generates. The two lines that make it hold are
the overflow-safe clamp in `Advance` (compare `e > room/rate` before multiplying,
never after) and the availability check in `TryTake` (guard the `uint64`
subtraction). Model-based fuzzing is what exercises the *interleavings* — a run of
takes that empties the bucket followed by a huge advance is exactly the path a
naive `tokens += elapsed*rate` overflows on, and a fixed test rarely lands on it.
`TestOverflowClamp` pins the worst case explicitly; the fuzz target proves it for
the sequences you did not enumerate. Run `go test -race ./...`, then
`go test -fuzz=FuzzBucketInvariant -fuzztime=2s`.

## Resources

- [`encoding/binary.Uvarint`](https://pkg.go.dev/encoding/binary#Uvarint) — decoding the fuzzed bytes into an operation stream.
- [Go Fuzzing reference](https://go.dev/doc/security/fuzz/) — why the fuzz body must be deterministic for the engine to reproduce and minimize a failing sequence.
- [Go integer overflow (spec: arithmetic operators)](https://go.dev/ref/spec#Arithmetic_operators) — unsigned wraparound, the hazard the clamp and the take-guard defend against.

---

Back to [08-path-traversal-safe-join.md](08-path-traversal-safe-join.md) | Next: [10-backoff-bounds.md](10-backoff-bounds.md)
