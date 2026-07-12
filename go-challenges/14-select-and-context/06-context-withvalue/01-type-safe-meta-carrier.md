# Exercise 1: Type-Safe Request-Metadata Carrier

Every downstream value-based observability hook in this lesson depends on one
small, correct primitive: a package that owns an unexported context key and
exposes a typed accessor pair. This exercise builds that carrier — a `Meta`
struct, a `type key struct{}`, `WithMeta`/`FromContext`, and a `FormatLog`
formatter that reads `Meta` straight from a context — and pins its full contract
with tests.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
requestmeta/                 independent module: example.com/requestmeta
  go.mod
  requestmeta.go             Meta; type key struct{}; WithMeta, FromContext, FormatLog
  cmd/
    demo/
      main.go                propagates Meta through a handleOrder call chain
  requestmeta_test.go        round-trip, missing, immutability/layering, collision,
                             FormatLog, WithCancel-child survival, ExampleWithMeta
```

Files: `requestmeta.go`, `cmd/demo/main.go`, `requestmeta_test.go`.
Implement: a `Meta` value type, an unexported `key struct{}`, `WithMeta(ctx, Meta) ctx`, `FromContext(ctx) (Meta, bool)`, and `FormatLog(ctx, msg) string`.
Test: round-trip equality, `ok=false` + zero `Meta` on empty context, parent immutability across layered `WithMeta`, string-key lookup returns `nil`, `FormatLog` with and without meta, survival through a `WithCancel` child, and a runnable `Example`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/06-context-withvalue/01-type-safe-meta-carrier/cmd/demo
cd go-solutions/14-select-and-context/06-context-withvalue/01-type-safe-meta-carrier
```

### Why the key is `struct{}` and the accessor returns comma-ok

`ctx.Value` compares keys with `==`, so the only safe key is one no other package
can name. `type key struct{}` is the canonical choice: it is unexported (no other
package can reference the type), zero-width (it costs nothing to store or compare),
and unconstructable from outside (no other package can produce a `key{}` to pass to
`Value`). A string key would collide the moment another package also used `"meta"`;
the `struct{}` key makes that collision a compile-time impossibility rather than a
runtime hazard.

`FromContext` returns `(Meta, bool)` rather than a bare `Meta` so callers never
write `ctx.Value(key{}).(Meta)` themselves — a naked assertion that would panic on
absence. The comma-ok form inside the accessor turns "no metadata" into a clean
`(Meta{}, false)`, which `FormatLog` handles by returning the message unadorned.
Storing the three fields as one `Meta` value under one key (rather than three
scalars under three keys) keeps the lookup a single O(1) comparison and gives one
place to evolve request metadata.

Create `requestmeta.go`:

```go
package requestmeta

import (
	"context"
	"fmt"
)

// Meta is the request-scoped data carried in the context. It is a value type
// (all fields are strings), so it is immutable and safe to share across the
// goroutines a request fans out into.
type Meta struct {
	TraceID string
	UserID  string
	Locale  string
}

// key is the unexported context-key type. Being unexported and zero-width, no
// other package can name or construct it, so cross-package collisions are
// structurally impossible.
type key struct{}

// WithMeta returns a new context carrying m. The parent is unchanged.
func WithMeta(parent context.Context, m Meta) context.Context {
	return context.WithValue(parent, key{}, m)
}

// FromContext extracts Meta from ctx. The second return is false when no Meta
// is present; it never panics on absence.
func FromContext(ctx context.Context) (Meta, bool) {
	m, ok := ctx.Value(key{}).(Meta)
	return m, ok
}

// FormatLog produces a one-line, request-correlated log entry. With no Meta in
// ctx it returns msg unadorned rather than emitting empty fields.
func FormatLog(ctx context.Context, msg string) string {
	m, ok := FromContext(ctx)
	if !ok {
		return msg
	}
	return fmt.Sprintf("[trace=%s user=%s locale=%s] %s", m.TraceID, m.UserID, m.Locale, msg)
}
```

### The demo: propagation through a call chain

The demo mints a `Meta` at the top of a request and shows that a function three
frames down (`handleOrder`) logs a fully correlated line without ever declaring a
`traceID` parameter. That is the whole point of request-scoped values: the trace
ID rides the context, not the signatures.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/requestmeta"
)

func main() {
	ctx := requestmeta.WithMeta(context.Background(), requestmeta.Meta{
		TraceID: "trace-001",
		UserID:  "user-42",
		Locale:  "en-US",
	})

	fmt.Println(requestmeta.FormatLog(ctx, "request received"))
	handleOrder(ctx, "ORD-1")
}

func handleOrder(ctx context.Context, orderID string) {
	fmt.Println(requestmeta.FormatLog(ctx, "handling order "+orderID))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[trace=trace-001 user=user-42 locale=en-US] request received
[trace=trace-001 user=user-42 locale=en-US] handling order ORD-1
```

### The contract tests

The tests pin every clause of the carrier's contract. `TestWithMetaStoresAndRetrieves`
is the round-trip. `TestFromContextMissingReturnsFalse` proves absence yields the
zero `Meta` and `false`. `TestWithValueIsImmutableAndLayered` proves the parent is
never mutated and that a second `WithMeta` shadows the first for descendants only.
`TestKeyTypeIsUnexported` proves a `string` key cannot reach the value stored under
`key{}`. `TestFormatLog*` pin the formatter. `TestSurvivesWithCancelChild` — the
reader's TODO, shipped here — proves a value set before a `WithCancel` still
resolves in the cancellable child, because the value list and the cancellation tree
share one parent chain.

Create `requestmeta_test.go`:

```go
package requestmeta

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestWithMetaStoresAndRetrieves(t *testing.T) {
	t.Parallel()

	m := Meta{TraceID: "abc", UserID: "u-42", Locale: "en-US"}
	ctx := WithMeta(context.Background(), m)

	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext: ok = false, want true")
	}
	if got != m {
		t.Fatalf("FromContext: %+v, want %+v", got, m)
	}
}

func TestFromContextMissingReturnsFalse(t *testing.T) {
	t.Parallel()

	got, ok := FromContext(context.Background())
	if ok {
		t.Fatalf("FromContext: ok = true (with %+v), want false", got)
	}
	if got != (Meta{}) {
		t.Fatalf("FromContext on empty: got %+v, want zero Meta", got)
	}
}

func TestWithValueIsImmutableAndLayered(t *testing.T) {
	t.Parallel()

	ctx0 := context.Background()
	ctx1 := WithMeta(ctx0, Meta{TraceID: "t1", UserID: "u1", Locale: "en"})
	ctx2 := WithMeta(ctx1, Meta{TraceID: "t1", UserID: "u1", Locale: "fr"})

	m1, _ := FromContext(ctx1)
	if m1.Locale != "en" {
		t.Fatalf("ctx1.Locale = %q, want en (parent must not change)", m1.Locale)
	}

	m2, _ := FromContext(ctx2)
	if m2.Locale != "fr" {
		t.Fatalf("ctx2.Locale = %q, want fr (child shadows parent)", m2.Locale)
	}

	if _, ok := FromContext(ctx0); ok {
		t.Fatal("ctx0 should not carry Meta")
	}
}

func TestKeyTypeIsUnexported(t *testing.T) {
	t.Parallel()

	ctx := WithMeta(context.Background(), Meta{TraceID: "t", UserID: "u", Locale: "l"})

	if v := ctx.Value("trace"); v != nil {
		t.Fatalf("ctx.Value(\"trace\") = %v, want nil (keys must be unexported types)", v)
	}
}

func TestFormatLogWithMeta(t *testing.T) {
	t.Parallel()

	ctx := WithMeta(context.Background(), Meta{TraceID: "abc", UserID: "u-1", Locale: "en"})
	got := FormatLog(ctx, "user signed in")
	want := "[trace=abc user=u-1 locale=en] user signed in"
	if got != want {
		t.Fatalf("FormatLog = %q, want %q", got, want)
	}
}

func TestFormatLogWithoutMeta(t *testing.T) {
	t.Parallel()

	got := FormatLog(context.Background(), "no metadata")
	if got != "no metadata" {
		t.Fatalf("FormatLog = %q, want %q", got, "no metadata")
	}
}

func TestFormatLogIncludesAllFields(t *testing.T) {
	t.Parallel()

	ctx := WithMeta(context.Background(), Meta{TraceID: "xyz", UserID: "u-2", Locale: "fr"})
	got := FormatLog(ctx, "msg")
	for _, want := range []string{"trace=xyz", "user=u-2", "locale=fr", "msg"} {
		if !strings.Contains(got, want) {
			t.Fatalf("FormatLog = %q, want to contain %q", got, want)
		}
	}
}

func TestSurvivesWithCancelChild(t *testing.T) {
	t.Parallel()

	m := Meta{TraceID: "t", UserID: "u", Locale: "en"}
	parent := WithMeta(context.Background(), m)

	child, cancel := context.WithCancel(parent)
	defer cancel()

	got, ok := FromContext(child)
	if !ok || got != m {
		t.Fatalf("FromContext(WithCancel child) = %+v,%v; want %+v,true", got, ok, m)
	}
}

func ExampleWithMeta() {
	ctx := WithMeta(context.Background(), Meta{TraceID: "abc", UserID: "u-1", Locale: "en"})
	fmt.Println(FormatLog(ctx, "hello"))
	// Output: [trace=abc user=u-1 locale=en] hello
}
```

## Review

The carrier is correct when three properties hold together. The key is an
unexported type, so `TestKeyTypeIsUnexported` sees `nil` for a string lookup — if
you had used a string key, that test would surface another package's value.
`FromContext` returns comma-ok, so absence is `(Meta{}, false)` and `FormatLog`
degrades to the bare message instead of emitting `[trace= user= locale=]`. And the
value is immutable: `WithMeta` never mutates its parent, so layering a second
`Meta` shadows the first for descendants only while `ctx1` keeps `en`. The
`WithCancel` test is the conceptual payoff — a value set before a cancellation node
still resolves below it, because `Value` walks the parent chain past any node type.
Run `go test -race` to confirm concurrent reads of the shared `Meta` are clean;
because `Meta` is an all-string value type, they are.

## Resources

- [context.WithValue](https://pkg.go.dev/context#WithValue) — the key-comparability and request-scoped-data rules.
- [Go Blog: Contexts and structs](https://go.dev/blog/context-and-structs) — when a value belongs in the context versus a struct field.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — the accessor-pair pattern and value immutability.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-request-id-middleware.md](02-request-id-middleware.md)
