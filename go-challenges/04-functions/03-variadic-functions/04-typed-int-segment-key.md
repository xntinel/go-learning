# Exercise 4: Typed Variadic Key from a Variable Number of IDs

Not every open set is strings. A pagination key or a shard key is built from a
variable number of *integers* — page numbers, shard ids, tenant ids. You build
`IntKey(prefix string, values ...int)` that converts each id with `strconv.Itoa`
and joins them with `:`. This is a typed (non-`any`) variadic: the compiler still
checks that every argument is an `int`, and you pay a convert-then-join loop.

This module is self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
intkey/                    independent module: example.com/intkey
  go.mod                   go 1.25
  intkey.go                IntKey(prefix string, values ...int) string
  cmd/
    demo/
      main.go              runnable demo: page and shard keys
  intkey_test.go           table tests: multi, zero-values, splat, negatives
```

- Files: `intkey.go`, `cmd/demo/main.go`, `intkey_test.go`.
- Implement: `IntKey(prefix string, values ...int) string` converting each value with `strconv.Itoa` and joining with `:`.
- Test: `IntKey("page",1,2,3)=="page:1:2:3"`; `IntKey("page")=="page:"`; a `[]int` splatted through a helper equals the inline call; negative ids format correctly.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/intkey/cmd/demo
cd ~/go-exercises/intkey
go mod init example.com/intkey
go mod edit -go=1.25
```

### A typed variadic and the convert-then-join loop

`values ...int` is a *typed* variadic: unlike `...any`, the compiler enforces that
every argument is an `int`, so a typo like `IntKey("page", "1")` is a compile
error, not a runtime surprise. That is the safety you keep by not reaching for
`...any` when the elements are genuinely homogeneous. The cost is that you cannot
hand a `[]int` straight to `strings.Join` (which wants `[]string`); you convert
each element with `strconv.Itoa` into a pre-sized `[]string`, then join. Pre-sizing
with `make([]string, 0, len(values))` avoids reallocation in the loop.

Two boundary cases define the contract. `IntKey("page")` with no ids yields
`page:` — the prefix, a `:`, and an empty join of zero values. That trailing colon
is a deliberate, tested shape (it distinguishes "the page namespace with no ids"
from a bare `page`). And negatives format naturally: `strconv.Itoa(-3)` is `-3`, so
`IntKey("shard", -3)` is `shard:-3`. Because the variadic is typed, the splat form
`IntKey("page", ids...)` for a `[]int` is exactly the inline form — the test proves
that equivalence through a small helper that builds the slice at runtime.

Create `intkey.go`:

```go
// intkey.go
package intkey

import (
	"strconv"
	"strings"
)

// IntKey builds a ':'-joined key from a prefix and a variable number of integer
// ids, e.g. IntKey("page", 1, 2, 3) == "page:1:2:3". With no ids it returns
// prefix + ":".
func IntKey(prefix string, values ...int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
	}
	return prefix + ":" + strings.Join(parts, ":")
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/intkey"
)

func main() {
	fmt.Println(intkey.IntKey("page", 1, 2, 3))
	fmt.Println(intkey.IntKey("page"))
	fmt.Println(intkey.IntKey("shard", -3))

	ids := []int{10, 20}
	fmt.Println(intkey.IntKey("tenant", ids...))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
page:1:2:3
page:
shard:-3
tenant:10:20
```

### Tests

`TestIntKeySplatEqualsInline` builds a `[]int` at runtime, splats it, and asserts
the result equals the inline call — the property that lets you assemble an id list
dynamically. The zero-values and negative cases pin the two boundary shapes.

Create `intkey_test.go`:

```go
// intkey_test.go
package intkey

import (
	"fmt"
	"testing"
)

func TestIntKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix string
		values []int
		want   string
	}{
		{"three ids", "page", []int{1, 2, 3}, "page:1:2:3"},
		{"zero values", "page", nil, "page:"},
		{"single id", "shard", []int{7}, "shard:7"},
		{"negative id", "shard", []int{-3}, "shard:-3"},
		{"mixed sign", "delta", []int{-1, 0, 1}, "delta:-1:0:1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IntKey(tc.prefix, tc.values...); got != tc.want {
				t.Fatalf("IntKey(%q, %v) = %q, want %q", tc.prefix, tc.values, got, tc.want)
			}
		})
	}
}

func TestIntKeySplatEqualsInline(t *testing.T) {
	t.Parallel()

	inline := IntKey("page", 1, 2, 3)

	ids := make([]int, 0, 3)
	for i := range 3 {
		ids = append(ids, i+1)
	}
	splatted := IntKey("page", ids...)

	if inline != splatted {
		t.Fatalf("inline %q != splatted %q", inline, splatted)
	}
}

func Example() {
	fmt.Println(IntKey("page", 1, 2, 3))
	// Output: page:1:2:3
}
```

## Review

`IntKey` is correct when it maps a prefix and an ordered list of ints to a
`:`-joined string, with `IntKey("page")` returning `page:` and negatives rendered
by `strconv.Itoa` verbatim. The design point is that a typed variadic keeps
compile-time safety the moment the elements are all one type — reach for `...any`
only when they genuinely are not. The convert-then-join loop, pre-sized, is the
standard shape whenever the variadic element type is not `string`. Run `go test
-race` to confirm.

## Resources

- [`strconv.Itoa`](https://pkg.go.dev/strconv#Itoa)
- [`strings.Join`](https://pkg.go.dev/strings#Join)
- [Effective Go: Variadic functions](https://go.dev/doc/effective_go#variadic-functions)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-deterministic-namespaced-hash.md](03-deterministic-namespaced-hash.md) | Next: [05-structured-log-attrs-helper.md](05-structured-log-attrs-helper.md)
