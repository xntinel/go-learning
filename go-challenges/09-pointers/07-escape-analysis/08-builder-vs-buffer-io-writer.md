# Exercise 8: String Building: strings.Builder vs io.Writer Escape

Assembling a string — a log line, a SQL fragment — has a fast monomorphic path
and a slower one that crosses an interface boundary. `strings.Builder` with `Grow`
builds the bytes in place and hands them back with no copy; writing through an
`io.Writer` parameter boxes the concrete writer, which escapes. This module builds
both, proves they emit identical bytes, and measures the gap.

This module is fully self-contained.

## What you'll build

```text
serializer/                   independent module: example.com/serializer
  go.mod                      go 1.26
  serialize.go                SerBuilder (strings.Builder + Grow),
                              WriteParts (io.Writer, noinline), SerToString
  cmd/
    demo/
      main.go                 serializes a SQL fragment both ways; shows the gap
  serialize_test.go           golden equality + AllocsPerRun (builder < writer)
```

Files: `serialize.go`, `cmd/demo/main.go`, `serialize_test.go`.
Implement: `SerBuilder(parts []string) string` using `strings.Builder.Grow`;
`WriteParts(w io.Writer, parts []string)` (the boxed path, `//go:noinline`); and
`SerToString` wrapping `WriteParts` around a `bytes.Buffer`.
Test: a golden test asserting both serializers emit byte-identical strings, and an
`AllocsPerRun` test asserting the builder path allocates fewer times than the
io.Writer path.
Verify: `go test -count=1 -race ./...`, then observe the interface escape with
`go build -gcflags=-m ./... 2>&1 | grep 'escapes to heap'`.

Set up the module:

```bash
mkdir -p ~/go-exercises/serializer/cmd/demo
cd ~/go-exercises/serializer
go mod init example.com/serializer
```

### The interface boundary is where the value escapes

`SerBuilder` is monomorphic: it computes the total length, calls `Grow` once to
reserve the whole backing array, writes each part with `WriteString`, and returns
`b.String()`. Two things make it lean. `Grow` collapses what would be several
growth reallocations into a single allocation of the backing array. And
`strings.Builder.String()` returns the accumulated bytes as a string *without
copying them* — it reinterprets the builder's buffer directly, which is safe
because a finished builder does not mutate its buffer again. The net cost is one
allocation: the backing array, which escapes only because it becomes the returned
string.

`WriteParts` takes an `io.Writer`. That is the fast path's opposite: to call
`w.Write`, the concrete writer you pass (`*bytes.Buffer`) must satisfy the
interface, and because the analyzer cannot see through the interface method call
to prove the writer does not outlive the frame, the concrete value escapes to the
heap at the boundary. Add the buffer's own growth and its `String()` copy, and the
io.Writer route allocates more than the builder. The lesson is not that `io.Writer`
is bad — it is the right abstraction when the destination varies — but that on a
tight, known-destination path, keeping it monomorphic with `strings.Builder`
avoids the interface-conversion escape.

Both routes must emit identical bytes, or the fast path is a behavior change. The
golden test enforces it.

Create `serialize.go`:

```go
package serialize

import (
	"bytes"
	"io"
	"strings"
)

// SerBuilder joins parts with single spaces using a strings.Builder sized once
// with Grow. String() returns the bytes without copying.
func SerBuilder(parts []string) string {
	n := 0
	for _, p := range parts {
		n += len(p) + 1
	}
	var b strings.Builder
	b.Grow(n)
	for i, p := range parts {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p)
	}
	return b.String()
}

// WriteParts writes the same joined output through an io.Writer. Passing a
// concrete writer to this interface parameter escapes it to the heap.
//
//go:noinline
func WriteParts(w io.Writer, parts []string) {
	for i, p := range parts {
		if i > 0 {
			io.WriteString(w, " ")
		}
		io.WriteString(w, p)
	}
}

// SerToString assembles the string via the io.Writer path, for comparison.
func SerToString(parts []string) string {
	var buf bytes.Buffer
	WriteParts(&buf, parts)
	return buf.String()
}
```

### The runnable demo

The demo serializes a parameterized SQL fragment both ways, proves the outputs are
identical, and reports that the builder path allocates fewer times. It prints the
builder's stable allocation count and a comparison boolean rather than the
version-sensitive writer count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"testing"

	"example.com/serializer"
)

func main() {
	parts := []string{"SELECT", "id,", "name", "FROM", "users", "WHERE", "tenant", "=", "$1"}

	built := serialize.SerBuilder(parts)
	written := serialize.SerToString(parts)
	fmt.Printf("builder: %s\n", built)
	fmt.Printf("writer:  %s\n", written)
	fmt.Printf("equal:   %v\n", built == written)

	var sink string
	bA := testing.AllocsPerRun(1000, func() { sink = serialize.SerBuilder(parts) })
	wA := testing.AllocsPerRun(1000, func() {
		var buf bytes.Buffer
		serialize.WriteParts(&buf, parts)
		sink = buf.String()
	})
	_ = sink
	fmt.Printf("builder allocs/op: %.0f\n", bA)
	fmt.Printf("builder cheaper than writer: %v\n", bA < wA)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
builder: SELECT id, name FROM users WHERE tenant = $1
writer:  SELECT id, name FROM users WHERE tenant = $1
equal:   true
builder allocs/op: 1
builder cheaper than writer: true
```

### Tests

`TestSerializersAgree` is the golden test: both routes must produce byte-identical
output across several inputs, so the builder fast path is a safe substitution.
`TestBuilderAllocatesLess` asserts the builder path allocates strictly fewer times
than the io.Writer path.

Create `serialize_test.go`:

```go
package serialize

import (
	"bytes"
	"fmt"
	"testing"
)

func TestSerializersAgree(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"SELECT", "1"},
		{"INSERT", "INTO", "t", "VALUES", "($1,", "$2)"},
		{"a", "b", "c", "d", "e"},
		{"single"},
	}
	for _, parts := range cases {
		if a, b := SerBuilder(parts), SerToString(parts); a != b {
			t.Errorf("mismatch for %v:\n builder=%q\n writer =%q", parts, a, b)
		}
	}
}

var sink string

func TestBuilderAllocatesLess(t *testing.T) {
	parts := []string{"SELECT", "id,", "name", "FROM", "users", "WHERE", "tenant", "=", "$1"}
	bA := testing.AllocsPerRun(1000, func() { sink = SerBuilder(parts) })
	wA := testing.AllocsPerRun(1000, func() {
		var buf bytes.Buffer
		WriteParts(&buf, parts)
		sink = buf.String()
	})
	if !(bA < wA) {
		t.Errorf("expected builder to allocate less: builder=%.1f writer=%.1f", bA, wA)
	}
}

func BenchmarkSerBuilder(b *testing.B) {
	parts := []string{"SELECT", "id,", "name", "FROM", "users"}
	b.ReportAllocs()
	for b.Loop() {
		sink = SerBuilder(parts)
	}
}

func BenchmarkSerToString(b *testing.B) {
	parts := []string{"SELECT", "id,", "name", "FROM", "users"}
	b.ReportAllocs()
	for b.Loop() {
		sink = SerToString(parts)
	}
}

func ExampleSerBuilder() {
	fmt.Println(SerBuilder([]string{"level=info", "msg=ok"}))
	// Output: level=info msg=ok
}
```

## Review

The serializers are correct only if they agree byte-for-byte; `TestSerializersAgree`
is what makes swapping the io.Writer path for the builder a pure optimization. The
allocation lesson lives at the interface boundary: `strings.Builder` keeps the path
monomorphic and pays one allocation for the backing array (returned as a zero-copy
string), while passing a concrete writer to an `io.Writer` parameter boxes it and
escapes, and the buffer adds growth and a copy on top. Confirm the escape with
`go build -gcflags=-m` and look for the `*bytes.Buffer` argument reported as
escaping to the heap. The mistake to avoid is threading `io.Writer` through a
hot, single-destination assembly path out of habit; when the destination is known
and you want a string, `strings.Builder` with `Grow` is the tighter tool.

## Resources

- [strings.Builder](https://pkg.go.dev/strings#Builder) — `Grow`, `WriteString`, and zero-copy `String`.
- [bytes.Buffer](https://pkg.go.dev/bytes#Buffer) — the io.Writer implementation used for comparison.
- [Go Blog: Escape analysis](https://go.dev/blog/escape-analysis) — interface-conversion escapes.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-large-struct-value-vs-pointer.md](07-large-struct-value-vs-pointer.md) | Next: [09-zero-alloc-hotpath-guard.md](09-zero-alloc-hotpath-guard.md)
