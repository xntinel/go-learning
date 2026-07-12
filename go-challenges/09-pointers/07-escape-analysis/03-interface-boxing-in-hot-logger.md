# Exercise 3: Interface Boxing: Why Structured-Log Args Escape

A structured logger that takes `fields ...any` is ergonomic and, on a per-request
path, quietly expensive: every non-pointer scalar you pass is boxed into an
interface value, and boxing usually escapes to the heap. This module builds two
renderers that emit byte-identical output — one variadic-`any`, one typed — and
measures the allocation gap so you can see boxing as a cost, not an abstraction.

This module is fully self-contained.

## What you'll build

```text
hotlogger/                    independent module: example.com/hotlogger
  go.mod                      go 1.26
  logfmt.go                   RenderAny(...any) (boxes each scalar) and
                              RenderTyped(tenant, count, ok) (no boxing)
  cmd/
    demo/
      main.go                 renders a request line both ways; shows slog attrs
  logfmt_test.go              golden-equality + AllocsPerRun gap (any > typed)
```

Files: `logfmt.go`, `cmd/demo/main.go`, `logfmt_test.go`.
Implement: `RenderAny(fields ...any) string` (key/value pairs via `fmt`) and
`RenderTyped(tenant string, count int, ok bool) string` (typed, no `...any`).
Test: a golden test asserting both emit identical strings, and an `AllocsPerRun`
test asserting the `any` renderer allocates strictly more than the typed one.
Verify: `go test -count=1 -race ./...`, then observe boxing with
`go build -gcflags=-m ./... 2>&1 | grep -E 'escapes to heap|does not escape'`.

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/07-escape-analysis/03-interface-boxing-in-hot-logger/cmd/demo
cd go-solutions/09-pointers/07-escape-analysis/03-interface-boxing-in-hot-logger
```

### Where the allocation comes from

`RenderAny(fields ...any)` looks harmless, but two escapes hide in the signature.
First, the variadic pack: calling `RenderAny("tenant", "acme", "count", n, ...)`
builds a `[]any` backing array, and because that slice is handed to a function the
analyzer treats conservatively, it can escape. Second, and more important, each
non-pointer scalar you place into an `any` slot is *boxed*: the runtime allocates
a spot for the concrete value plus a type word. Passing a `string` boxes a
two-word header; passing a large or runtime-computed `int` (here `count` is
`1<<20`) allocates. Then `fmt.Fprintf("%v=%v", ...)` reflects over each boxed
value, adding its own allocations. Every one of these is per-call, per-argument —
multiply by your request rate.

`RenderTyped(tenant string, count int, ok bool)` takes the same three fields with
their real types. No `...any`, no boxing, no reflection. It writes into a
`strings.Builder` sized once with `Grow`, converts the int with
`strconv.AppendInt` into a stack scratch buffer, and returns the assembled string.
The int never becomes an `interface`, so it never allocates for boxing. The
measured gap is the price of the ergonomic signature.

Both renderers must produce the *same* bytes, or the optimization is a behavior
change rather than a speedup. The golden test enforces that: the typed fast path
is only safe because it is observably equivalent.

The idiomatic production answer is a typed attribute API. `log/slog` is built on
exactly this idea: `slog.Int("count", n)` and `slog.String("tenant", v)` return a
typed `slog.Attr` that carries the scalar without boxing it into a bare `any`. The
demo shows those attrs rendering to the same `key=value` text.

Create `logfmt.go`:

```go
package logfmt

import (
	"fmt"
	"strconv"
	"strings"
)

// RenderAny formats alternating key/value pairs. Each non-pointer value is boxed
// into the any slot and reflected over by fmt, so scalars allocate per call.
func RenderAny(fields ...any) string {
	var b strings.Builder
	b.Grow(64)
	for i := 0; i+1 < len(fields); i += 2 {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%v=%v", fields[i], fields[i+1])
	}
	return b.String()
}

// RenderTyped formats the same request line with concrete types. No value is
// boxed into an interface, so no per-argument heap allocation occurs.
func RenderTyped(tenant string, count int, ok bool) string {
	var b strings.Builder
	b.Grow(64)
	b.WriteString("tenant=")
	b.WriteString(tenant)
	b.WriteString(" count=")
	var scratch [20]byte
	b.Write(strconv.AppendInt(scratch[:0], int64(count), 10))
	b.WriteString(" ok=")
	b.WriteString(strconv.FormatBool(ok))
	return b.String()
}
```

### The runnable demo

The demo renders a request-log line both ways, prints them to prove equality, and
then shows the `log/slog` typed attributes that production code uses to avoid
boxing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log/slog"

	"example.com/hotlogger"
)

func main() {
	const count = 1 << 20
	a := logfmt.RenderAny("tenant", "acme", "count", count, "ok", true)
	t := logfmt.RenderTyped("acme", count, true)
	fmt.Println("any:  ", a)
	fmt.Println("typed:", t)
	fmt.Println("equal:", a == t)

	line := slog.String("tenant", "acme").String() + " " +
		slog.Int("count", count).String() + " " +
		slog.Bool("ok", true).String()
	fmt.Println("slog: ", line)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
any:   tenant=acme count=1048576 ok=true
typed: tenant=acme count=1048576 ok=true
equal: true
slog:  tenant=acme count=1048576 ok=true
```

### Tests

`TestRenderersAgree` is the safety net: the typed fast path is only valid because
it emits the same bytes as the `any` path, across several field sets.
`TestTypedAllocatesLess` is the point: it asserts the `any` renderer allocates
strictly more than the typed one, and that the typed one stays within a small
budget. The comparison is relative (heavier path allocates more), which is robust
across Go versions and the race detector where absolute counts shift.

Create `logfmt_test.go`:

```go
package logfmt

import "testing"

func TestRenderersAgree(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tenant string
		count  int
		ok     bool
	}{
		{"acme", 1 << 20, true},
		{"globex", 0, false},
		{"initech", 42, true},
	}
	for _, tc := range cases {
		any := RenderAny("tenant", tc.tenant, "count", tc.count, "ok", tc.ok)
		typed := RenderTyped(tc.tenant, tc.count, tc.ok)
		if any != typed {
			t.Errorf("mismatch for %+v:\n any   = %q\n typed = %q", tc, any, typed)
		}
	}
}

var sink string

func TestTypedAllocatesLess(t *testing.T) {
	const count = 1 << 20
	anyAllocs := testing.AllocsPerRun(1000, func() {
		sink = RenderAny("tenant", "acme", "count", count, "ok", true)
	})
	typedAllocs := testing.AllocsPerRun(1000, func() {
		sink = RenderTyped("acme", count, true)
	})
	if !(anyAllocs > typedAllocs) {
		t.Errorf("expected boxing to allocate more: any=%.1f typed=%.1f", anyAllocs, typedAllocs)
	}
	if typedAllocs > 2 {
		t.Errorf("typed path allocs/op = %.1f, want <= 2", typedAllocs)
	}
}

func BenchmarkRenderAny(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sink = RenderAny("tenant", "acme", "count", 1<<20, "ok", true)
	}
}

func BenchmarkRenderTyped(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sink = RenderTyped("acme", 1<<20, true)
	}
}
```

## Review

The renderers are correct only if they agree byte-for-byte; `TestRenderersAgree`
is what makes the typed fast path a safe substitution rather than a behavior
change. The allocation gap is the lesson: the `any` path pays for the variadic
pack, for boxing each scalar into an interface, and for `fmt`'s reflection, while
the typed path boxes nothing. Confirm the boxing directly with
`go build -gcflags=-m` — you will see the arguments to `RenderAny` reported as
escaping to the heap. The mistake this guards against is a per-request logger or
metrics call built on `...any` and `fmt`: it reads cleanly and profiles terribly.
Reach for typed attributes (`slog.Int`, `slog.String`) or preformatted keys on
the hot path.

## Resources

- [log/slog: Attr and typed constructors](https://pkg.go.dev/log/slog#Attr) — `slog.Int`, `slog.String`, `slog.Bool`.
- [Go Blog: Escape analysis](https://go.dev/blog/escape-analysis) — why interface boxing escapes.
- [strings.Builder](https://pkg.go.dev/strings#Builder) — `Grow` and zero-copy `String`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-observe-escapes-in-tests.md](02-observe-escapes-in-tests.md) | Next: [04-sync-pool-buffer-reuse.md](04-sync-pool-buffer-reuse.md)
