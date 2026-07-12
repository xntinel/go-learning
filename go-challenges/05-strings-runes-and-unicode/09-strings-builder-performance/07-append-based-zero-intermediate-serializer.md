# Exercise 7: Zero-Intermediate-Alloc Serialization with the strconv.Append* Family

The hottest serialization paths — a per-record writer in an export loop, a metrics
sample formatter — can drop below even `strings.Builder` by writing directly into a
caller-owned `[]byte` that is reused across calls. This exercise builds such a
serializer with the `strconv.Append*` family and `append`, proves it is byte-identical
to a `fmt.Sprintf` reference, and shows it reaching 0 allocations per call on the
steady-state path.

This module is self-contained.

## What you'll build

```text
appendser/                   independent module: example.com/appendser
  go.mod
  appendser.go               Record.AppendTo (strconv.Append*), Sprintf reference
  cmd/
    demo/
      main.go                serializes two records into one reused buffer, prints them
  appendser_test.go          byte-identity vs Sprintf, control-char quoting, 0-alloc benchmark
```

Files: `appendser.go`, `cmd/demo/main.go`, `appendser_test.go`.
Implement: `(Record).AppendTo(dst []byte) []byte` using `strconv.AppendInt/AppendQuote/AppendFloat/AppendUint`, and a `Sprintf` reference producing the same bytes.
Test: `AppendTo(nil)` equals the `fmt.Sprintf` reference for representative and control-character inputs; a benchmark with a reused `dst` shows 0 allocs/op.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/09-strings-builder-performance/07-append-based-zero-intermediate-serializer/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/09-strings-builder-performance/07-append-based-zero-intermediate-serializer
```

### The append idiom and what it buys

Every function in the `strconv.Append*` family has the same shape: it takes a
destination `[]byte`, formats a value onto the end of it, and returns the extended
slice — `dst = strconv.AppendInt(dst, r.ID, 10)`. The built-in `append` fills the
gaps for literal bytes like the separators. The value of returning the slice is that
the caller owns the backing array: on the next call you pass `dst[:0]`, which keeps
the same array and length-zeros it, so once the array is big enough to hold a record
you never allocate again. That is the steady-state 0-alloc property, and it is
strictly below what `strings.Builder` can do, because a Builder allocates its own
buffer each construction, whereas here the buffer lives across calls.

Why is this faster than `fmt.Sprintf`, beyond the allocation? `fmt` works through the
`any` interface: every argument is boxed into an interface value (which can itself
allocate), then reflected on at runtime to decide how to format it. `strconv.AppendInt`
knows statically that it is formatting an `int64` — no boxing, no reflection, just a
tight digit loop into your buffer. The cost you pay is ergonomic: you thread `dst`
through every call and must remember to reset it with `dst[:0]`, which is why this
tier is reserved for paths a profiler has flagged, not for every string you build.

Correctness is anchored to a reference you trust: `fmt.Sprintf("%d %q %.2f %d", ...)`.
`strconv.AppendQuote` implements exactly the same quoting as `%q` (both are
`strconv.Quote`), `AppendFloat` with `'f'`/prec 2 matches `%.2f`, and `AppendInt`/
`AppendUint` match `%d`. So the append version must be byte-for-byte identical to the
`Sprintf` version — the test asserts that over ordinary input and over a string full
of control characters, where `%q`/`AppendQuote` escape `\t`, `\n`, and embedded
quotes identically.

Create `appendser.go`:

```go
package appendser

import (
	"fmt"
	"strconv"
)

// Record is one row to serialize.
type Record struct {
	ID    int64
	Name  string
	Score float64
	Tags  uint64
}

// AppendTo formats the record onto dst and returns the extended slice. Passing
// dst[:0] on each call reuses the backing array, so the steady-state path
// allocates nothing.
func (r Record) AppendTo(dst []byte) []byte {
	dst = strconv.AppendInt(dst, r.ID, 10)
	dst = append(dst, ' ')
	dst = strconv.AppendQuote(dst, r.Name)
	dst = append(dst, ' ')
	dst = strconv.AppendFloat(dst, r.Score, 'f', 2, 64)
	dst = append(dst, ' ')
	dst = strconv.AppendUint(dst, r.Tags, 10)
	return dst
}

// Sprintf is the reference implementation using fmt. It produces the same bytes
// as AppendTo but boxes each argument through the any interface and reflects on
// it at runtime.
func (r Record) Sprintf() string {
	return fmt.Sprintf("%d %q %.2f %d", r.ID, r.Name, r.Score, r.Tags)
}
```

### The runnable demo

The demo serializes two records into a single buffer that is reused via `dst[:0]`,
showing the intended calling pattern.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/appendser"
)

func main() {
	records := []appendser.Record{
		{ID: 1, Name: "alice", Score: 9.5, Tags: 7},
		{ID: 2, Name: "bob\tsmith", Score: 3.14159, Tags: 42},
	}

	dst := make([]byte, 0, 64)
	for _, r := range records {
		dst = r.AppendTo(dst[:0])
		fmt.Println(string(dst))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1 "alice" 9.50 7
2 "bob\tsmith" 3.14 42
```

### Tests

The identity test asserts `AppendTo(nil)` equals `Sprintf()` for representative and
control-character records. The benchmark reuses one `dst` across iterations and, run
with `-benchmem`, reports 0 allocs/op — the payoff of the append idiom.

Create `appendser_test.go`:

```go
package appendser

import (
	"fmt"
	"testing"
)

func TestAppendMatchesSprintf(t *testing.T) {
	t.Parallel()

	cases := []Record{
		{ID: 1, Name: "alice", Score: 9.5, Tags: 7},
		{ID: -20, Name: "", Score: 0, Tags: 0},
		{ID: 99, Name: "tab\tnewline\nquote\"end", Score: 3.14159, Tags: 18446744073709551615},
	}
	for _, r := range cases {
		got := string(r.AppendTo(nil))
		want := r.Sprintf()
		if got != want {
			t.Fatalf("AppendTo = %q, Sprintf = %q", got, want)
		}
	}
}

func TestReuseBackingArray(t *testing.T) {
	t.Parallel()

	dst := make([]byte, 0, 64)
	dst = Record{ID: 1, Name: "a", Score: 1, Tags: 1}.AppendTo(dst[:0])
	first := string(dst)
	dst = Record{ID: 2, Name: "b", Score: 2, Tags: 2}.AppendTo(dst[:0])
	second := string(dst)

	if first != `1 "a" 1.00 1` {
		t.Fatalf("first = %q", first)
	}
	if second != `2 "b" 2.00 2` {
		t.Fatalf("second = %q (reuse corrupted the buffer)", second)
	}
}

func BenchmarkAppendReused(b *testing.B) {
	r := Record{ID: 1, Name: "alice", Score: 9.5, Tags: 7}
	dst := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		dst = r.AppendTo(dst[:0])
	}
	_ = dst
}

func BenchmarkSprintf(b *testing.B) {
	r := Record{ID: 1, Name: "alice", Score: 9.5, Tags: 7}
	b.ReportAllocs()
	for b.Loop() {
		_ = r.Sprintf()
	}
}

func ExampleRecord_AppendTo() {
	r := Record{ID: 7, Name: "svc", Score: 1.5, Tags: 3}
	fmt.Println(string(r.AppendTo(nil)) == `7 "svc" 1.50 3`)
	// Output: true
}
```

## Review

The serializer is correct when `AppendTo(nil)` is byte-identical to the `Sprintf`
reference, including the control-character case where `AppendQuote` and `%q` must
escape `\t`, `\n`, and embedded quotes the same way. The performance claim is the
0 allocs/op the reused-`dst` benchmark shows: because the caller owns the backing
array and resets it with `dst[:0]`, no allocation happens once the array is large
enough. Reserve this tier for hot paths — for cold, one-shot formatting the `Sprintf`
version is clearer and its allocation does not matter. The idiom's discipline is the
`dst[:0]` reset; forget it and you append onto stale bytes.

## Resources

- [strconv.AppendQuote](https://pkg.go.dev/strconv#AppendQuote) — Go-quoted string into a byte slice, same as `%q`.
- [strconv.AppendInt](https://pkg.go.dev/strconv#AppendInt) — integer formatting without reflection.
- [strconv.AppendFloat](https://pkg.go.dev/strconv#AppendFloat) — float formatting into a byte slice.

---

Prev: [06-buffer-pool-hot-path.md](06-buffer-pool-hot-path.md) | Back to [00-concepts.md](00-concepts.md) | Next: [08-sse-frame-writer-streaming.md](08-sse-frame-writer-streaming.md)
