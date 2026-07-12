# Exercise 4: Eliminate Interface-Boxing Allocations On A Logging Hot Path

Structured loggers love the signature `log(fields ...any)` — it accepts anything.
On a request hot path it also allocates on every scalar you pass, because boxing a
non-pointer value into an `any` copies it to the heap. This module measures that
cost and reworks the path to typed fields that do not box.

This module is fully self-contained: its own module, code, demo, and tests.

## What you'll build

```text
logfields/                  independent module: example.com/logfields
  go.mod                    go 1.26
  logfields.go              AppendAny (boxes) vs AppendTyped (typed Field, no box)
  cmd/
    demo/
      main.go               builds one record each way, prints them
  logfields_test.go         AllocsPerRun asserts the reduction; a ReportAllocs benchmark
```

- Files: `logfields.go`, `cmd/demo/main.go`, `logfields_test.go`.
- Implement: `AppendAny(dst []any, vals ...int64) []any` (the naive boxing path) and `AppendTyped(dst []Field, vals ...int64) []Field` (typed fields, no boxing).
- Test: `testing.AllocsPerRun` asserts the naive path boxes at least one alloc per scalar and the typed path allocates fewer; a benchmark with `ReportAllocs` and `b.Loop` documents allocs/op.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why boxing allocates, and when it does not

Putting an `int64` into an `any` requires the interface's data word to point at
the value. A pointer-typed value is free — the pointer already exists — but a
non-pointer value needs storage the data word can reference. Escape analysis
decides where: if the interface escapes the call (here it is appended into a slice
that is returned and read later), the value is copied to the heap, one allocation
per boxing. `AppendAny` boxes each `int64` at `append(dst, v)`, so a call with
three values costs three allocations.

`AppendTyped` sidesteps it. A `Field{Int: v}` stores the `int64` in a struct
field with no interface conversion, so appending `Field` values into a
`[]Field` — whose backing array already has capacity — allocates nothing. The
scalar never becomes an `any`, so it never has to escape to the heap for the data
word.

Two honesty notes the test respects. First, the runtime keeps a small-integer
cache: boxing values in roughly `[0, 256)` reuses static storage and does *not*
allocate, which is why the test uses values well above that range. Second, boxing
a word-sized value does not *always* allocate — if escape analysis proves the
interface stays on the stack, it is free — so the assertion targets a *measured
reduction* between the two paths, not a hardcoded zero. The point being proven is
relative: the typed path allocates strictly fewer than the boxing path on the same
workload.

Create `logfields.go`:

```go
package logfields

import "fmt"

// Field is a typed log field. Keeping the scalar in a struct field avoids
// converting it to an any, which is what forces the boxing allocation.
type Field struct {
	Key string
	Int int64
}

// String renders the field for a log line.
func (f Field) String() string {
	return fmt.Sprintf("%s=%d", f.Key, f.Int)
}

// AppendAny is the naive hot path: every scalar is boxed into an any as it is
// appended, forcing a heap allocation per non-cached value.
func AppendAny(dst []any, vals ...int64) []any {
	for _, v := range vals {
		dst = append(dst, v) // boxing: int64 -> any
	}
	return dst
}

// AppendTyped is the optimized hot path: scalars stay in typed Field values, so
// no boxing allocation occurs.
func AppendTyped(dst []Field, vals ...int64) []Field {
	for _, v := range vals {
		dst = append(dst, Field{Int: v})
	}
	return dst
}
```

### The runnable demo

The demo builds one record each way from the same three values and prints them, so
you can see the two produce equivalent data — the difference is only in how much
the hot path allocates, which the tests measure.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logfields"
)

func main() {
	vals := []int64{1000, 2000, 3000}

	anyRec := logfields.AppendAny(make([]any, 0, 8), vals...)
	fmt.Println("boxed any record:", anyRec)

	typedRec := logfields.AppendTyped(make([]logfields.Field, 0, 8), vals...)
	fmt.Print("typed field record: ")
	for i, f := range typedRec {
		if i > 0 {
			fmt.Print(" ")
		}
		fmt.Print(f.String())
	}
	fmt.Println()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
boxed any record: [1000 2000 3000]
typed field record: =1000 =2000 =3000
```

The typed fields carry an empty `Key`, so each renders as `=<n>`; the demo shows
the value carried through each path, not a populated key set.

### Tests

`TestBoxingAllocations` is the measurement. It reuses a pre-sized backing slice
across runs (`dst[:0]` keeps the capacity so `append` never reallocates the array)
so the *only* allocations left to count are the boxings. The values come from a
runtime slice, not constants, so the compiler cannot fold them into static
storage. `AllocsPerRun` then reports the average: the naive path allocates one per
scalar (three), and the typed path allocates zero. The assertion is relative —
naive is at least three, typed is strictly fewer — to stay honest about
escape-analysis variability. `BenchmarkAppend` documents allocs/op with
`ReportAllocs` and the Go 1.24 `b.Loop` form.

Create `logfields_test.go`:

```go
package logfields

import "testing"

func TestBoxingAllocations(t *testing.T) {
	vals := []int64{1000, 2000, 3000} // runtime values, above the small-int cache

	bufAny := make([]any, 0, 8)
	naive := testing.AllocsPerRun(1000, func() {
		bufAny = AppendAny(bufAny[:0], vals...)
	})

	bufTyped := make([]Field, 0, 8)
	typed := testing.AllocsPerRun(1000, func() {
		bufTyped = AppendTyped(bufTyped[:0], vals...)
	})

	if naive < float64(len(vals)) {
		t.Fatalf("naive path allocated %.1f, want >= %d (one box per scalar)", naive, len(vals))
	}
	if typed >= naive {
		t.Fatalf("typed path allocated %.1f, not fewer than naive %.1f", typed, naive)
	}
	t.Logf("boxing allocs: naive=%.1f typed=%.1f", naive, typed)
}

func BenchmarkAppend(b *testing.B) {
	vals := []int64{1000, 2000, 3000}

	b.Run("any", func(b *testing.B) {
		b.ReportAllocs()
		buf := make([]any, 0, 8)
		for b.Loop() {
			buf = AppendAny(buf[:0], vals...)
		}
	})

	b.Run("typed", func(b *testing.B) {
		b.ReportAllocs()
		buf := make([]Field, 0, 8)
		for b.Loop() {
			buf = AppendTyped(buf[:0], vals...)
		}
	})
}
```

## Review

The measurement is the artifact here, and it is correct when it survives the two
traps that hide boxing costs. If the values were untyped constants, the compiler
would fold `any(1000)` into static read-only storage and the naive path would
falsely measure zero — hence a runtime `[]int64`. If the values were in `[0, 256)`
the small-integer cache would absorb the boxing and again read zero — hence values
of 1000 and up. With those controlled, `AppendAny` allocates one per scalar and
`AppendTyped` allocates none, and the assertion proves the *reduction* rather than
a brittle absolute. On a real logger this is the difference between a per-request
allocation storm visible as GC pressure in pprof and a hot path that stays on the
stack. Keep the assertion relative: escape analysis can legitimately change the
absolute numbers between toolchain versions.

## Resources

- [testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — the average-allocations measurement this test asserts on.
- [testing.B.Loop](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop used with `ReportAllocs`.
- [Go blog: Profiling Go Programs](https://go.dev/blog/pprof) — how boxing allocations show up as GC pressure in a real profile.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-interface-comparability-panic.md](03-interface-comparability-panic.md) | Next: [05-optional-interface-upgrade.md](05-optional-interface-upgrade.md)
