# Exercise 2: Forwarding a Variadic `...any` Through fmt.Sprintf

Sometimes a cache key needs a formatted segment — `order.42`, not two separate
fields. The idiomatic shape for that is the same one `fmt.Printf` uses: a format
string followed by a variadic `...any`. You build `Builder.Stringf(format, args
...any)` that renders one segment with `fmt.Sprintf` and feeds it through the same
`join` as Exercise 1, and you learn what splatting `...any` into another variadic
costs you at the type checker.

This module is self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
keyfmt/                    independent module: example.com/keyfmt
  go.mod                   go 1.25
  cachekey.go              Builder with String and Stringf(format, args ...any)
  cmd/
    demo/
      main.go              runnable demo: formatted segments
  cachekey_test.go         table tests: Stringf output, prebuilt []any equivalence
```

- Files: `cachekey.go`, `cmd/demo/main.go`, `cachekey_test.go`.
- Implement: `Stringf(format string, args ...any) string` that returns `join([]string{fmt.Sprintf(format, args...)})`.
- Test: `Stringf("order.%d", 42)=="orders:order.42"`; a pre-built `[]any` splatted with `args...` produces the identical result as inline arguments.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/02-printf-style-arg-forwarding/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/02-printf-style-arg-forwarding
go mod edit -go=1.25
```

### The format-plus-variadic contract, and splatting into another variadic

`Stringf` deliberately mirrors `fmt.Sprintf`'s signature: `(format string, args
...any)`. This is the canonical Go shape for "a template plus its fill-ins", and
copying it means callers already know how to use `Stringf` — the verbs, the arg
order, the escaping are all `fmt`'s. Under the hood there are *two* variadic hops:
`Stringf` receives `args ...any`, and then forwards it to `fmt.Sprintf` by
splatting — `fmt.Sprintf(format, args...)`. That `args...` passes `Stringf`'s
slice header straight into `Sprintf`; because `Sprintf` only reads the arguments,
the aliasing is harmless here. The rendered segment is then wrapped in a
one-element slice and handed to the shared `join`, so the namespace prefix and
`:` separator behave exactly as in `String`.

The cost of `...any` is that the compiler stops checking verb/argument agreement.
`fmt.Sprintf("order.%d", "not-a-number")` compiles cleanly and produces
`order.%!d(string=not-a-number)` at runtime. The guard is `go vet`: its `printf`
analyzer recognizes `Sprintf`-family functions and flags a mismatched verb at
build time. It fires on this illustrative call (do not add it to the package — it
would fail the gate's `go vet` step, which is exactly the point):

```text
// go vet flags this: %d wants an int, got a string
_ = fmt.Sprintf("order.%d", "oops")
// vet: fmt.Sprintf format %d has arg "oops" of wrong type string
```

Because the analyzer keys on the function *name*, a wrapper like `Stringf` is not
automatically analyzed — vet does not know `Stringf`'s first parameter is a format
string. You can teach it by naming the method so vet's heuristics recognize it, or
you simply keep the `fmt.Sprintf` call visible inside `Stringf` (as here) so the
analyzer sees the real format call it understands.

Create `cachekey.go`:

```go
// cachekey.go
package cachekey

import (
	"fmt"
	"strings"
)

// Builder produces namespace-prefixed cache keys.
type Builder struct {
	namespace string
}

// New returns a Builder that prefixes every key with namespace.
func New(namespace string) *Builder {
	return &Builder{namespace: namespace}
}

// String joins the namespace and segments with ':'.
func (b *Builder) String(parts ...string) string {
	return b.join(parts)
}

// Stringf renders one segment with fmt.Sprintf, then prepends the namespace.
// It mirrors the (format, args ...any) shape of the fmt family.
func (b *Builder) Stringf(format string, args ...any) string {
	return b.join([]string{fmt.Sprintf(format, args...)})
}

func (b *Builder) join(parts []string) string {
	all := make([]string, 0, len(parts)+1)
	all = append(all, b.namespace)
	all = append(all, parts...)
	return strings.Join(all, ":")
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/keyfmt"
)

func main() {
	orders := cachekey.New("orders")
	fmt.Println(orders.Stringf("order.%d", 42))
	fmt.Println(orders.Stringf("%s.%d", "invoice", 7))

	// A pre-built []any splatted in produces the identical result.
	args := []any{"invoice", 7}
	fmt.Println(orders.Stringf("%s.%d", args...))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
orders:order.42
orders:invoice.7
orders:invoice.7
```

Note the module directory is `keyfmt` but the package is `cachekey`; the import
path is the module path, and the package name is what you reference in code.

### Tests

`TestStringfEqualsPrebuiltArgs` proves the property that matters for forwarding:
inline arguments and a splatted `[]any` are the same call. That equivalence is why
you can build an argument list dynamically and pass it on.

Create `cachekey_test.go`:

```go
// cachekey_test.go
package cachekey

import (
	"fmt"
	"testing"
)

func TestStringf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		ns     string
		format string
		args   []any
		want   string
	}{
		{"int verb", "orders", "order.%d", []any{42}, "orders:order.42"},
		{"string and int", "orders", "%s.%d", []any{"invoice", 7}, "orders:invoice.7"},
		{"no verbs", "cfg", "static", nil, "cfg:static"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := New(tc.ns)
			if got := b.Stringf(tc.format, tc.args...); got != tc.want {
				t.Fatalf("Stringf(%q, %v) = %q, want %q", tc.format, tc.args, got, tc.want)
			}
		})
	}
}

func TestStringfEqualsPrebuiltArgs(t *testing.T) {
	t.Parallel()

	b := New("orders")
	inline := b.Stringf("%s.%d", "invoice", 7)

	args := []any{"invoice", 7}
	splatted := b.Stringf("%s.%d", args...)

	if inline != splatted {
		t.Fatalf("inline %q != splatted %q", inline, splatted)
	}
}

func Example() {
	b := New("orders")
	fmt.Println(b.Stringf("order.%d", 42))
	// Output: orders:order.42
}
```

## Review

`Stringf` is correct when it behaves as `fmt.Sprintf` with a namespace glued on:
the verbs are `fmt`'s, the args flow through unchanged whether inline or splatted,
and a formatted segment joins under the same `:` rule as a literal one. The senior
takeaway is the trade you made: `...any` bought ergonomics at the cost of
compile-time verb checking, and `go vet`'s printf analyzer is the replacement
guard. Keep the real `fmt.Sprintf` call inside the method so vet can see and check
the format string, and never let a mismatched-verb call reach production
unanalyzed.

## Resources

- [`fmt` package: printing and format verbs](https://pkg.go.dev/fmt)
- [`go vet` and the printf analyzer](https://pkg.go.dev/cmd/vet)
- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-cache-key-builder-variadic.md](01-cache-key-builder-variadic.md) | Next: [03-deterministic-namespaced-hash.md](03-deterministic-namespaced-hash.md)
