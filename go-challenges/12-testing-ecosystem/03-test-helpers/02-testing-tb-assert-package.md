# Exercise 2: A Generic Assert Package Reusable in Tests and Benchmarks

A service that shares one internal `assert` package across its `*_test.go` files
writes assertions once and reuses them everywhere — including inside benchmarks
and fuzz targets. The trick is to type the helpers over a minimal interface that
`*testing.T`, `*testing.B`, and a recording fake all satisfy, because
`testing.TB` itself is sealed and cannot be faked. This module builds that
package and proves the helpers report correctly by unit-testing them.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
tbassert/                    independent module: example.com/tbassert
  go.mod                     go 1.26
  assert/
    assert.go                TB interface; RequireNoError, Equal, SliceEqual, Less
  cmd/
    demo/
      main.go                drives the helpers through a recording TB
  assert/
    assert_test.go           unit-tests the helpers via a fake recorder; a benchmark proving TB reuse
```

- Files: `assert/assert.go`, `cmd/demo/main.go`, `assert/assert_test.go`.
- Implement: a minimal `TB` interface (`Helper`, `Fatalf`) plus `RequireNoError`, `Equal[T comparable]`, `SliceEqual[T comparable]`, and `Less[T cmp.Ordered]`, all typed over `TB`.
- Test: pass a fake recorder implementing `TB` and assert each helper fires (or does not) correctly; then use the real helpers inside a `BenchmarkXxx` to prove `*testing.B` satisfies the same interface.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/03-test-helpers/02-testing-tb-assert-package/assert go-solutions/12-testing-ecosystem/03-test-helpers/02-testing-tb-assert-package/cmd/demo
cd go-solutions/12-testing-ecosystem/03-test-helpers/02-testing-tb-assert-package
```

### Why a minimal `TB` interface, not `testing.TB`

You cannot write a fake that implements `testing.TB`: the interface has an
unexported method, so only the `testing` package can produce values that satisfy
it. That is a deliberate seal. It means an assertion helper typed as
`func Equal[T comparable](t testing.TB, ...)` is reusable across `TestXxx`,
`BenchmarkXxx`, and `FuzzXxx` — but *un-unit-testable*, because you cannot hand it
anything except a real `*testing.T`.

The idiomatic resolution, used by testify's `TestingT` and countless internal
`assert` packages, is to type the helpers over a *tiny* interface that lists only
the methods the helpers actually call:

```go
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}
```

`*testing.T`, `*testing.B`, and `*testing.F` all satisfy this — they have both
methods — so the helpers remain reusable across tests, benchmarks, and fuzz
targets. And a five-line fake recorder satisfies it too, so you can unit-test the
helpers themselves. The one behavioral note: on a real `*testing.T`, `Fatalf`
calls `runtime.Goexit`, which stops the test; on the fake it merely records and
returns. So the helpers must be written to *return* right after `Fatalf` (never
assume it aborts), which they naturally do.

### The helpers

`RequireNoError` fails when the error is non-nil — the single most common
assertion in any backend test. `Equal[T comparable]` and `SliceEqual[T
comparable]` compare values; `SliceEqual` delegates to `slices.Equal` rather than
hand-rolling a loop. `Less[T cmp.Ordered]` demonstrates that the same pattern
extends to ordered comparisons for benchmarks over numeric results. Each calls
`t.Helper()` first so a failure is attributed to the calling test line.

Create `assert/assert.go`:

```go
package assert

import (
	"cmp"
	"slices"
)

// TB is the minimal surface the helpers need. *testing.T, *testing.B, and
// *testing.F all satisfy it, and so can a fake recorder for unit tests —
// unlike the sealed testing.TB, which cannot be implemented outside testing.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// RequireNoError fails the test if err is non-nil.
func RequireNoError(t TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Equal fails the test unless got == want.
func Equal[T comparable](t TB, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// SliceEqual fails the test unless got and want have equal elements in order.
func SliceEqual[T comparable](t TB, got, want []T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("slices differ: got %v, want %v", got, want)
	}
}

// Less fails the test unless a < b under the natural ordering of T.
func Less[T cmp.Ordered](t TB, a, b T) {
	t.Helper()
	if !(a < b) {
		t.Fatalf("expected %v < %v", a, b)
	}
}
```

### The runnable demo

The demo cannot import `testing`'s `*T`, so it drives the helpers through the same
kind of fake recorder the tests use — which doubles as a demonstration that the
`TB` interface is genuinely open. It runs one passing and one failing assertion
and prints what the recorder saw.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/tbassert/assert"
)

// recorder is a fake assert.TB: it records the first Fatalf instead of aborting.
type recorder struct {
	helpers int
	failed  bool
	msg     string
}

func (r *recorder) Helper() { r.helpers++ }

func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}

func main() {
	pass := &recorder{}
	assert.Equal(pass, 42, 42)
	fmt.Printf("equal(42,42): failed=%v\n", pass.failed)

	fail := &recorder{}
	assert.RequireNoError(fail, errors.New("db down"))
	fmt.Printf("requireNoError(db down): failed=%v msg=%q\n", fail.failed, fail.msg)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
equal(42,42): failed=false
requireNoError(db down): failed=true msg="unexpected error: db down"
```

### The tests

The tests unit-test the helpers themselves. A `recorder` fake implements `TB`;
each subtest drives a helper with matching and mismatching inputs and asserts the
recorder's `failed` flag and that `Helper` was called. Then `BenchmarkSum` uses
the *real* `Equal` and `Less` helpers with `*testing.B` — which compiles only
because `*testing.B` satisfies `TB`, proving the reuse claim at build time.

Create `assert/assert_test.go`:

```go
package assert

import (
	"errors"
	"fmt"
	"testing"
)

// recorder is a fake TB that records instead of aborting, so we can unit-test
// the helpers themselves.
type recorder struct {
	helpers int
	failed  bool
	msg     string
}

func (r *recorder) Helper() { r.helpers++ }

func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}

func TestRequireNoError(t *testing.T) {
	t.Parallel()
	t.Run("nil error passes", func(t *testing.T) {
		t.Parallel()
		r := &recorder{}
		RequireNoError(r, nil)
		if r.failed {
			t.Fatalf("RequireNoError(nil) reported failure: %q", r.msg)
		}
		if r.helpers == 0 {
			t.Fatal("helper did not call t.Helper()")
		}
	})
	t.Run("non-nil error fails", func(t *testing.T) {
		t.Parallel()
		r := &recorder{}
		RequireNoError(r, errors.New("boom"))
		if !r.failed {
			t.Fatal("RequireNoError(err) did not report failure")
		}
	})
}

func TestEqual(t *testing.T) {
	t.Parallel()
	t.Run("equal passes", func(t *testing.T) {
		t.Parallel()
		r := &recorder{}
		Equal(r, "a", "a")
		if r.failed {
			t.Fatalf("Equal(a,a) reported failure: %q", r.msg)
		}
	})
	t.Run("unequal fails", func(t *testing.T) {
		t.Parallel()
		r := &recorder{}
		Equal(r, 1, 2)
		if !r.failed {
			t.Fatal("Equal(1,2) did not report failure")
		}
	})
}

func TestSliceEqual(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	SliceEqual(r, []int{1, 2, 3}, []int{1, 2, 3})
	if r.failed {
		t.Fatalf("SliceEqual on identical slices failed: %q", r.msg)
	}
	r2 := &recorder{}
	SliceEqual(r2, []int{1, 2}, []int{1, 2, 3})
	if !r2.failed {
		t.Fatal("SliceEqual on differing slices did not fail")
	}
}

func TestLess(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	Less(r, 1, 2)
	if r.failed {
		t.Fatalf("Less(1,2) failed: %q", r.msg)
	}
	r2 := &recorder{}
	Less(r2, 2, 2)
	if !r2.failed {
		t.Fatal("Less(2,2) did not fail")
	}
}

// sum is a trivial function under benchmark; the point is the assertion reuse.
func sum(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}

// BenchmarkSum uses the real Equal and Less helpers with *testing.B, proving
// *testing.B satisfies the same TB interface *testing.T does.
func BenchmarkSum(b *testing.B) {
	xs := []int{1, 2, 3, 4}
	Equal(b, sum(xs), 10)
	Less(b, 0, sum(xs))
	for range b.N {
		_ = sum(xs)
	}
}

func ExampleEqual() {
	r := &recorder{}
	Equal(r, 3, 4)
	fmt.Println(r.failed, r.msg)
	// Output: true got 3, want 4
}
```

## Review

The package is correct when every helper (a) calls `t.Helper()` first, (b) fires
exactly on the mismatch condition, and (c) is typed over `TB` rather than
`*testing.T`. The unit tests prove (b) by driving the fake recorder through both
outcomes; the `recorder.helpers` check proves (a) for the representative helper;
and `BenchmarkSum` proves (c) at compile time — if you had typed the helpers as
`*testing.T`, that benchmark would not build. The single most common mistake here
is reaching for `testing.TB` in the helper signature "because it is the real
interface" and then discovering the helpers cannot be unit-tested at all; the
minimal `TB` interface is the deliberate fix. Run `go test -race -bench=. -count=1`
to exercise the benchmark alongside the unit tests.

## Resources

- [testing.TB](https://pkg.go.dev/testing#TB) — the sealed interface and why it cannot be implemented externally.
- [slices.Equal](https://pkg.go.dev/slices#Equal) — element-wise comparison used by `SliceEqual`.
- [cmp.Ordered](https://pkg.go.dev/cmp#Ordered) — the constraint behind the `Less` helper.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-httptest-server-harness.md](03-httptest-server-harness.md)
