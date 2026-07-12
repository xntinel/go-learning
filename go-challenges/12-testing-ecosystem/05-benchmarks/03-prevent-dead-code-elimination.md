# Exercise 3: Defeat Compiler Dead-Code Elimination with a Sink

The single most common way to publish a fake benchmark is to discard the result of
the thing you are benchmarking, let the compiler delete the call, and report the
resulting sub-nanosecond number as a fast function. This module reproduces that bug
on a pure hash function — the kind you would benchmark when choosing a checksum for a
request body — and fixes it two ways: a package-level sink and `b.Loop`.

## What you'll build

```text
hashing/                   independent module: example.com/hashing
  go.mod                   go 1.24
  hashing.go               FNV1a(data []byte) uint64; SHA256Hex(data []byte) string
  cmd/
    demo/
      main.go              runnable demo: hash a fixed request body, print the digests
  hashing_test.go          known-answer correctness tests; BenchmarkFNV1aSink (sink),
                           BenchmarkFNV1aLoop (b.Loop); Example
```

- Files: `hashing.go`, `cmd/demo/main.go`, `hashing_test.go`.
- Implement: `FNV1a` over `hash/fnv` and `SHA256Hex` over `crypto/sha256`.
- Test: known-answer hashes of a fixed input, plus the sink and `b.Loop` benchmarks.
- Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem`.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/05-benchmarks/03-prevent-dead-code-elimination/cmd/demo
cd go-solutions/12-testing-ecosystem/05-benchmarks/03-prevent-dead-code-elimination
go mod edit -go=1.24
```

### The bug, and why the number is impossible

Here is the naive benchmark, the one that lies. Do not assemble it; it is here to be
recognized and rejected:

```text
// WRONG: the result is discarded, so the compiler may delete the FNV1a call
func BenchmarkFNV1aNaive(b *testing.B) {
	data := []byte("POST /api/v1/orders 200")
	for range b.N {
		FNV1a(data) // return value dropped -> dead code -> deleted
	}
}
// reports something like: BenchmarkFNV1aNaive-8  1000000000  0.31 ns/op
```

`0.31 ns/op` is faster than a single L1 cache load (roughly 1 ns) and far faster than
hashing 23 bytes could ever be. That is the tell: the number is not a fast function,
it is a deleted function. Go's compiler performs dead-code elimination — if it can
prove the result of `FNV1a(data)` is never observed, the call has no effect, so it is
removed, and the loop body becomes empty. You measured the cost of an empty loop.

There are two correct fixes, and this module ships both. The first assigns the result
to a *package-level* variable named `sink`. Package scope matters: a local variable
the compiler can still often prove dead, but a package-level variable could be read by
any other code in the package, so the compiler must keep the assignment — and
therefore the hash — alive. The second fix is `for b.Loop()`, which keeps the loop's
results alive for you as part of its contract; no sink is needed. A correct benchmark
of `FNV1a` over a 23-byte input reports a plausible few nanoseconds per op, not a
fraction of one.

`FNV1a` wraps `hash/fnv`'s 64-bit FNV-1a; `SHA256Hex` wraps `crypto/sha256.Sum256`
and hex-encodes the digest. Both are pure functions of their input, which is exactly
what makes them vulnerable to elimination and exactly what makes them good subjects
for this lesson.

Create `hashing.go`:

```go
package hashing

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
)

// FNV1a returns the 64-bit FNV-1a hash of data. It is a pure function, which makes
// its benchmark a prime target for dead-code elimination if the result is discarded.
func FNV1a(data []byte) uint64 {
	h := fnv.New64a()
	h.Write(data)
	return h.Sum64()
}

// SHA256Hex returns the lowercase hex-encoded SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/hashing"
)

func main() {
	body := []byte("POST /api/v1/orders 200")
	fmt.Printf("fnv1a  = %d\n", hashing.FNV1a(body))
	fmt.Printf("sha256 = %s\n", hashing.SHA256Hex(body))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fnv1a  = 5993163354892537708
sha256 = be176344acc720080f44fb979ef43d9437db319bbbec4c49f443a1a706318d43
```

### Tests

The correctness tests are known-answer tests: the FNV-1a and SHA-256 of a fixed input
are constants, so the test pins them exactly. The benchmarks demonstrate the two
defenses; both hash the same fixed body, and both should report a plausible non-trivial
`ns/op` rather than the deleted-code fraction.

Create `hashing_test.go`:

```go
package hashing

import (
	"fmt"
	"testing"
)

var body = []byte("POST /api/v1/orders 200")

func TestFNV1aKnownAnswer(t *testing.T) {
	t.Parallel()
	const want = uint64(5993163354892537708)
	if got := FNV1a(body); got != want {
		t.Fatalf("FNV1a = %d, want %d", got, want)
	}
}

func TestSHA256KnownAnswer(t *testing.T) {
	t.Parallel()
	const want = "be176344acc720080f44fb979ef43d9437db319bbbec4c49f443a1a706318d43"
	if got := SHA256Hex(body); got != want {
		t.Fatalf("SHA256Hex = %s, want %s", got, want)
	}
}

// sink is package-level so the compiler cannot prove the assignment is dead and
// therefore cannot delete the hash computation in BenchmarkFNV1aSink.
var sink uint64

func BenchmarkFNV1aSink(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		sink = FNV1a(body) // observed by package scope -> not eliminated
	}
}

func BenchmarkFNV1aLoop(b *testing.B) {
	b.ReportAllocs()
	var h uint64
	for b.Loop() {
		h = FNV1a(body) // b.Loop keeps h alive -> not eliminated
	}
	_ = h
}

func ExampleFNV1a() {
	fmt.Println(FNV1a([]byte("POST /api/v1/orders 200")))
	// Output: 5993163354892537708
}
```

Run the benchmarks; both fixed variants report a plausible cost, unlike the naive one:

```bash
go test -bench=. -benchmem
```

```text
BenchmarkFNV1aSink-8    47281003     25.4 ns/op     32 B/op    1 allocs/op
BenchmarkFNV1aLoop-8    46903118     25.6 ns/op     32 B/op    1 allocs/op
PASS
```

## Review

The known-answer tests make the hashes correct and stable; if a refactor accidentally
switches FNV-1a for FNV-1 (a one-line ordering difference), the constant catches it.
The benchmark lesson is the reflex you must build: a sub-nanosecond `ns/op` on a
function that touches memory is never a fast function, it is a deleted one. The two
fixes are both idiomatic — a package-level `sink` for the `b.N` form you will find in
old code, and `b.Loop` for new code, which removes the need to think about sinks at
all. The `1 allocs/op` comes from `fnv.New64a()` returning a `hash.Hash` on the heap;
that is real and worth seeing, and it is why a hot path that hashes constantly might
prefer the allocation-free `hash/maphash` or a reused hasher — a decision a benchmark
like this one surfaces.

## Resources

- [`hash/fnv`](https://pkg.go.dev/hash/fnv) — `New64a` and the FNV-1a construction.
- [`crypto/sha256.Sum256`](https://pkg.go.dev/crypto/sha256#Sum256) — the fixed-size digest used here.
- [`testing.B.Loop`](https://pkg.go.dev/testing#B.Loop) — keeps results alive so the optimizer cannot delete benchmarked work.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-b-loop-modern-benchmark.md](02-b-loop-modern-benchmark.md) | Next: [04-exclude-setup-with-timers.md](04-exclude-setup-with-timers.md)
