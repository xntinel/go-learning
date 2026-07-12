# Exercise 10: Avoid the strings.Builder Footguns: copylocks, Reuse, and Escape Semantics

`strings.Builder` is easy to use wrong in ways the compiler will not stop you from
writing but `go vet` and `go test -race` will. This exercise reproduces the four
classic misuses — copying a Builder by value, reusing one without `Reset`, sharing one
across goroutines, and the `make([]byte,0,n)`+`string()` anti-pattern — and builds the
corrected, vet-clean, race-free versions that a response formatter should ship.

This module is self-contained.

## What you'll build

```text
footguns/                    independent module: example.com/footguns
  go.mod
  footguns.go                Format (*strings.Builder), Reusable (Reset per call)
  cmd/
    demo/
      main.go                formats a couple of lines and reuses a Reusable
  footguns_test.go           reuse-no-bleed-through, concurrent -race safety
```

Files: `footguns.go`, `cmd/demo/main.go`, `footguns_test.go`.
Implement: `Format(fields []string) string` taking `*strings.Builder` internally, and a `Reusable` type that `Reset`s a `bytes.Buffer` on each call.
Test: reuse does not bleed prior content; concurrent formatting is race-free under `-race`.
Verify: `go vet ./...` is clean and `go test -count=1 -race ./...` passes.

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/09-strings-builder-performance/10-builder-copylocks-and-reset-footguns/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/09-strings-builder-performance/10-builder-copylocks-and-reset-footguns
```

### The four footguns, and why the compiler lets them through

**Copying a Builder by value.** A `strings.Builder` stores a pointer to itself to
detect illegal copies; passing or returning it by value duplicates that pointer and
corrupts the check, and later writes can panic or misbehave. The compiler accepts the
code, but `go vet`'s copylocks analyzer flags it:

```go
// Wrong: takes strings.Builder by value; go vet reports
//   "writeFields passes lock by value: strings.Builder contains ..."
func writeFields(b strings.Builder, fields []string) {
	for _, f := range fields {
		b.WriteString(f) // writes to a COPY; also a copylocks violation
	}
}
```

The fix is to always pass `*strings.Builder`. This is not a style preference; the
by-value form is a latent corruption that vet exists to catch, so the corrected code
below takes a pointer.

**Reusing without Reset.** Keeping a buffer around to avoid re-allocating is fine, but
you must `Reset` it before each reuse or the previous output bleeds into the next:

```go
// Wrong: no Reset, so the second call's output starts with the first call's bytes.
var shared bytes.Buffer

func formatLeaky(fields []string) string {
	for _, f := range fields {
		shared.WriteString(f)
	}
	return shared.String() // second call returns first-call bytes + these
}
```

**Sharing one Builder across goroutines.** A Builder (or Buffer) is not safe for
concurrent use; two goroutines writing the same one is a data race that `-race`
reports:

```go
// Wrong: every goroutine writes the same Builder -> data race under -race.
var raced strings.Builder

func handler(fields []string) { // called from many goroutines
	for _, f := range fields {
		raced.WriteString(f)
	}
}
```

**The make+string anti-pattern.** Pre-sizing a byte slice and converting at the end
does not save the allocation you think it does:

```go
// Wrong: string(b) allocates a NEW array and copies, defeating the pre-allocation.
func slow(fields []string) string {
	b := make([]byte, 0, 256)
	for _, f := range fields {
		b = append(b, f...)
	}
	return string(b) // one more alloc + copy here
}
```

The corrected serializer collapses all four: it takes `*strings.Builder`, `Reset`s a
reused `bytes.Buffer` between calls, keeps one buffer per goroutine (or a fresh Builder
per call), and uses `strings.Builder` + `Grow` so the single copy-free `String()` is
the finish instead of a second-allocating `string(b)`.

Create `footguns.go`:

```go
package footguns

import (
	"bytes"
	"strings"
)

// Format assembles fields into a fresh strings.Builder. writeFields takes a
// *strings.Builder (never by value), so go vet's copylocks analyzer is satisfied.
func Format(fields []string) string {
	var b strings.Builder
	b.Grow(len(fields) * 8)
	writeFields(&b, fields)
	return b.String()
}

// writeFields takes *strings.Builder. Passing strings.Builder by value would be
// a copylocks violation and could corrupt the Builder's internal state.
func writeFields(b *strings.Builder, fields []string) {
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(f)
	}
}

// Reusable holds a bytes.Buffer reused across calls to avoid re-allocating. Each
// call Resets first so no bytes bleed from the previous assembly. Use one
// Reusable per goroutine; it is not safe for concurrent use.
type Reusable struct {
	buf bytes.Buffer
}

// Format resets the buffer and assembles a fresh line into it.
func (r *Reusable) Format(fields []string) string {
	r.buf.Reset()
	for i, f := range fields {
		if i > 0 {
			r.buf.WriteByte(' ')
		}
		r.buf.WriteString(f)
	}
	return r.buf.String()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/footguns"
)

func main() {
	fmt.Println(footguns.Format([]string{"GET", "/api", "200"}))

	var r footguns.Reusable
	fmt.Println(r.Format([]string{"first", "call", "with", "many", "words"}))
	fmt.Println(r.Format([]string{"short"}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /api 200
first call with many words
short
```

### Tests

The reuse test formats a long input then a short one on the same `Reusable` and asserts
the short output is exactly `"short"` — no bleed-through, which is the proof `Reset`
runs. The concurrency test gives each goroutine its own `Reusable` (and also exercises
the fresh-Builder `Format`), and passes under `-race` because no buffer is shared.

Create `footguns_test.go`:

```go
package footguns

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestReuseNoBleedThrough(t *testing.T) {
	t.Parallel()

	var r Reusable
	_ = r.Format([]string{"a", "much", "longer", "first", "line"})
	if got := r.Format([]string{"short"}); got != "short" {
		t.Fatalf("bleed-through: got %q, want %q", got, "short")
	}
}

func TestConcurrentFormatIsRaceFree(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Fresh Builder per call: safe.
			want := "req " + strconv.Itoa(i)
			if got := Format([]string{"req", strconv.Itoa(i)}); got != want {
				t.Errorf("Format = %q, want %q", got, want)
			}

			// Own Reusable per goroutine: safe (never shared).
			var r Reusable
			if got := r.Format([]string{"g", strconv.Itoa(i)}); got != "g "+strconv.Itoa(i) {
				t.Errorf("Reusable.Format = %q", got)
			}
		}()
	}
	wg.Wait()
}

func ExampleFormat() {
	fmt.Println(Format([]string{"a", "b", "c"}))
	// Output: a b c
}
```

## Review

The corrected code is vet-clean and race-free precisely because it avoids the four
footguns: `writeFields` takes `*strings.Builder` so copylocks has nothing to flag,
`Reusable.Format` calls `Reset` first so no prior bytes bleed through, each goroutine
uses its own buffer so `-race` sees no shared writes, and `Format` uses
`strings.Builder`+`Grow`+`String()` rather than `make([]byte,0,n)`+`string(b)`, whose
final conversion would allocate and copy again. Confirm the two guarantees directly:
`go vet ./...` prints nothing, and `go test -race` passes. If you rewrite `writeFields`
to take `strings.Builder` by value, vet will flag it — that is the analyzer doing its
job.

## Resources

- [go vet: copylocks](https://pkg.go.dev/cmd/vet) — the analyzer that flags copying a `strings.Builder`.
- [strings.Builder](https://pkg.go.dev/strings#Builder) — the copy warning and `Reset` semantics.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — how `-race` catches shared-buffer writes.

---

Prev: [09-canonical-querystring-signing-builder.md](09-canonical-querystring-signing-builder.md) | Back to [00-concepts.md](00-concepts.md) | Next: [../10-building-a-text-processing-pipeline/00-concepts.md](../10-building-a-text-processing-pipeline/00-concepts.md)
