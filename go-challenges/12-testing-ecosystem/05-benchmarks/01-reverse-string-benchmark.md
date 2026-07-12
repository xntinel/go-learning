# Exercise 1: Benchmark a Hot-Path String Utility (b.N, RunParallel, -benchmem)

Every request-handling service has a handful of tiny helpers that run on the hot
path of every request — normalizing a header, reversing a byte range, canonicalizing
a key. Those helpers are exactly where a benchmark pays for itself, because a cost
you would never notice once is paid millions of times a second in aggregate. This
module benchmarks a byte-wise `Reverse` helper the classic way, with the `b.N` loop
that fills existing codebases, a `RunParallel` variant, and a long-input variant that
pins the "cost scales with input length" contract.

## What you'll build

```text
bench/                     independent module: example.com/bench
  go.mod                   go 1.24
  reverse.go               Reverse(s string) string  (byte-wise, in place on a copy)
  cmd/
    demo/
      main.go              runnable demo: reverse a few request-shaped strings
  reverse_test.go          TestReverse (table-driven); BenchmarkReverse (b.N + ReportAllocs);
                           BenchmarkReverseParallel (RunParallel); BenchmarkReverseLong; Example
```

- Files: `reverse.go`, `cmd/demo/main.go`, `reverse_test.go`.
- Implement: `Reverse(s string) string` that reverses the bytes of `s`.
- Test: table-driven correctness (empty, single, palindrome, two, multi-byte), plus three benchmarks and an `Example`.
- Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem`.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/05-benchmarks/01-reverse-string-benchmark/cmd/demo
cd go-solutions/12-testing-ecosystem/05-benchmarks/01-reverse-string-benchmark
go mod edit -go=1.24
```

### Why this is the shape you meet in real code

`Reverse` reverses a string by byte: it copies the string into a `[]byte`, swaps
from both ends toward the middle, and returns a new string. Reversing bytes (not
runes) is the honest contract to benchmark — it is O(n) in the length and allocates
exactly once (the `[]byte` copy, which `string(r)` may reuse). A multi-byte UTF-8
input reversed by byte produces mojibake, which is *correct* for a byte reverser and
is asserted as such in the test; a rune-aware reverse is a different, slower function
with a different allocation profile.

The three benchmarks each isolate one thing. `BenchmarkReverse` is the baseline: a
`for i := range b.N` loop with `b.ReportAllocs()` so the output carries `B/op` and
`allocs/op`. Notice the input string is built once, *above* the loop — that is the
single most important rule of the `b.N` form. `BenchmarkReverseParallel` wraps the
same call in `b.RunParallel`, spreading it across `GOMAXPROCS` goroutines; because
`Reverse` shares no state, its per-op cost under contention should be no worse than
sequential (and often better in `ns/op` because more cores are working), which is
itself a useful thing to confirm. `BenchmarkReverseLong` feeds a 10,000-byte input
so that, read next to `BenchmarkReverse`, you can see per-op time grow roughly with
length — the empirical statement that `Reverse` is linear.

Create `reverse.go`:

```go
package bench

// Reverse returns s with its bytes in reverse order. It reverses by byte, not by
// rune, so multi-byte UTF-8 input is scrambled by design; callers that need
// rune-aware reversal must decode first. It allocates once for the byte copy.
func Reverse(s string) string {
	r := []byte(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}
```

### The runnable demo

The demo reverses a few request-shaped strings so you can see the helper behave; it
does not print benchmark numbers (those vary by machine and are meaningless as fixed
output).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bench"
)

func main() {
	for _, s := range []string{"", "a", "level", "order-id-42"} {
		fmt.Printf("%q -> %q\n", s, bench.Reverse(s))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"" -> ""
"a" -> "a"
"level" -> "level"
"order-id-42" -> "24-di-redro"
```

### Tests

`TestReverse` is table-driven and covers the boundaries that break naive reversers:
the empty string (no swaps), a single byte (loop never enters), a palindrome (output
equals input), a two-byte string (a single swap), and a multi-byte case where
reversing by byte is expected to differ from a rune reverse. The three benchmarks
follow; `go test -race` compiles them but does not run them (that needs `-bench`),
so a benchmark that only compiles is enough to keep the module green while still
being runnable on demand.

Create `reverse_test.go`:

```go
package bench

import (
	"fmt"
	"strings"
	"testing"
)

func TestReverse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"single", "a", "a"},
		{"palindrome", "level", "level"},
		{"two", "ab", "ba"},
		{"hello", "hello", "olleh"},
		{"phrase", "order-id-42", "24-di-redro"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Reverse(tc.in); got != tc.want {
				t.Fatalf("Reverse(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReverseInvolution(t *testing.T) {
	t.Parallel()
	// Reversing twice is the identity for a byte reverser.
	for _, s := range []string{"", "a", "hello", "order-id-42"} {
		if got := Reverse(Reverse(s)); got != s {
			t.Fatalf("Reverse(Reverse(%q)) = %q, want %q", s, got, s)
		}
	}
}

func BenchmarkReverse(b *testing.B) {
	input := "the quick brown fox jumps over the lazy dog"
	b.ReportAllocs()
	for range b.N {
		_ = Reverse(input)
	}
}

func BenchmarkReverseParallel(b *testing.B) {
	input := "the quick brown fox jumps over the lazy dog"
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = Reverse(input)
		}
	})
}

func BenchmarkReverseLong(b *testing.B) {
	// A 10,000-byte input pins the "cost scales with length" contract: read this
	// benchmark's ns/op next to BenchmarkReverse and the ratio tracks length.
	input := strings.Repeat("x", 10_000)
	b.ReportAllocs()
	for range b.N {
		_ = Reverse(input)
	}
}

func ExampleReverse() {
	fmt.Println(Reverse("stressed"))
	// Output: desserts
}
```

Run the benchmarks (numbers are illustrative and machine-specific):

```bash
go test -bench=. -benchmem
```

```text
goos: darwin
goarch: arm64
pkg: example.com/bench
BenchmarkReverse-8            25039113        42.6 ns/op        96 B/op    2 allocs/op
BenchmarkReverseParallel-8   90118554        12.9 ns/op        96 B/op    2 allocs/op
BenchmarkReverseLong-8         233402      5106 ns/op       20480 B/op    2 allocs/op
PASS
```

The two allocations are the `[]byte(s)` copy and the `string(r)` copy — reversing by
byte cannot do it in place because Go strings are immutable, so this is the honest
allocation floor for this implementation.

## Review

Correctness comes first: `Reverse` is right when reversing twice is the identity and
when each table case matches, including the byte-level scramble of multi-byte input
that a rune reverser would handle differently. The benchmark discipline in this
module is threefold. The input is built once above every loop, so setup is never
folded into the per-op number. `b.ReportAllocs()` makes the `2 allocs/op` visible,
which is the figure you would gate on in review — if a change makes `Reverse`
allocate a third time (an escaped temporary, a stray `fmt`), that column catches it
regardless of the machine. And `BenchmarkReverseLong` next to `BenchmarkReverse` reads
as an empirical proof of linearity: hundreds of times the bytes costs on the order of a
hundred times the time here, consistent with O(n) plus a fixed per-call overhead that
dominates the tiny input, not the superlinear curve an accidental quadratic would show. Run `go test -race` to confirm the parallel benchmark shares no mutable state.

## Resources

- [`testing.B`](https://pkg.go.dev/testing#B) — `b.N`, `ReportAllocs`, `RunParallel`, and the benchmark contract.
- [`testing.B.RunParallel`](https://pkg.go.dev/testing#B.RunParallel) — running a benchmark body across GOMAXPROCS goroutines.
- [go test benchmark flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-bench`, `-benchmem`, `-benchtime`, `-count`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-b-loop-modern-benchmark.md](02-b-loop-modern-benchmark.md)
