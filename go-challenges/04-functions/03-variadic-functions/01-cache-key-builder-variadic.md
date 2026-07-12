# Exercise 1: Deterministic Cache-Key Builder with a Variadic Segment API

A cache in front of a database needs a key per request shape, and the natural way
to spell "a namespace plus any number of segments" is a variadic entry point. You
build a `cachekey.Builder` whose `String(parts ...string)` prepends a tenant
namespace and joins the segments with `:` into a deterministic key.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
cachekey/                  independent module: example.com/cachekey
  go.mod                   go 1.25
  cachekey.go              type Builder; New, String(parts ...string), join([]string)
  cmd/
    demo/
      main.go              runnable demo: build a few keys
  cachekey_test.go         table tests: join, zero-args, empty segment, no-mutation
```

- Files: `cachekey.go`, `cmd/demo/main.go`, `cachekey_test.go`.
- Implement: `New(namespace)` and `String(parts ...string) string` built on a shared unexported `join([]string) string` that pre-sizes its slice and never mutates the input.
- Test: `String("alice","42")=="users:alice:42"`; `String()` returns the bare namespace; `String("alice","","42")` keeps the empty segment; the passed variadic slice is never mutated.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/01-cache-key-builder-variadic/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/01-cache-key-builder-variadic
go mod edit -go=1.25
```

### Why a variadic entry point over a shared slice core

`String` is the public sugar: a caller writes `b.String("users", userID)` without
first materializing a `[]string`. Internally, every method funnels into one
unexported `join([]string)` — the single place that prepends the namespace and
joins with `:`. Centralizing the join means the namespace rule and the separator
live in exactly one spot, so `String`, and later `Stringf` and `Hash`, cannot
drift apart.

Two details make `join` correct under senior scrutiny. First, it *pre-sizes* the
result slice with `make([]string, 0, len(parts)+1)` — one slot for the namespace
plus one per segment — so the append never reallocates. Second, it *copies* the
segments into a fresh slice rather than mutating `parts`; because a caller may have
splatted a slice into `String`, writing into `parts` would corrupt the caller's
memory. `join` owns its output array and leaves the input untouched.

The empty-segment case is a deliberate contract, not an accident. `String("alice",
"", "42")` yields `users:alice::42` — the empty segment is preserved. For a cache
key an empty segment is a *real* segment (a present-but-empty field is a distinct
request shape from a missing field), so dropping it would collapse two different
requests onto one key. The zero-argument case, `String()`, returns just the
namespace, because joining a one-element slice produces that single element.

Create `cachekey.go`:

```go
// cachekey.go
package cachekey

import "strings"

// Builder produces deterministic, namespace-prefixed cache keys. The namespace
// isolates one tenant's keys from another's.
type Builder struct {
	namespace string
}

// New returns a Builder that prefixes every key with namespace.
func New(namespace string) *Builder {
	return &Builder{namespace: namespace}
}

// String joins the namespace and the given segments with ':'. Calling it with no
// segments returns the bare namespace. Empty segments are preserved.
func (b *Builder) String(parts ...string) string {
	return b.join(parts)
}

// join is the single place that prepends the namespace and joins with ':'. It
// pre-sizes its result (namespace + one slot per segment) and never mutates parts.
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

	"example.com/cachekey"
)

func main() {
	users := cachekey.New("users")
	fmt.Println(users.String("alice", "42"))
	fmt.Println(users.String())
	fmt.Println(users.String("alice", "", "42"))

	segs := []string{"eu-west", "shard7", "page1"}
	fmt.Println(users.String(segs...))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
users:alice:42
users
users:alice::42
users:eu-west:shard7:page1
```

### Tests

`TestJoinDoesNotMutateInput` is the one that proves the aliasing safety: it
splats a slice into `String`, then asserts the original slice is byte-for-byte
unchanged. If `join` ever appended into `parts` in place, this test would catch
the caller-corruption bug.

Create `cachekey_test.go`:

```go
// cachekey_test.go
package cachekey

import (
	"fmt"
	"slices"
	"testing"
)

func TestString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		namespace string
		parts     []string
		want      string
	}{
		{"two segments", "users", []string{"alice", "42"}, "users:alice:42"},
		{"zero segments", "orders", nil, "orders"},
		{"empty segment kept", "users", []string{"alice", "", "42"}, "users:alice::42"},
		{"single segment", "cfg", []string{"eu-west"}, "cfg:eu-west"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := New(tc.namespace)
			if got := b.String(tc.parts...); got != tc.want {
				t.Fatalf("String(%q) = %q, want %q", tc.parts, got, tc.want)
			}
		})
	}
}

func TestJoinDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	b := New("users")
	parts := []string{"alice", "42"}
	original := slices.Clone(parts)

	_ = b.String(parts...)

	if !slices.Equal(parts, original) {
		t.Fatalf("String mutated its input: got %v, want %v", parts, original)
	}
}

func TestJoinPreSizesExactly(t *testing.T) {
	t.Parallel()

	// A three-segment key joins namespace + 3 parts into 4 colon-separated fields.
	b := New("users")
	got := b.String("a", "b", "c")
	if want := "users:a:b:c"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
}

func Example() {
	b := New("users")
	fmt.Println(b.String("alice", "42"))
	// Output: users:alice:42
}
```

## Review

The builder is correct when the key is a pure function of the namespace and the
ordered segments: `String("alice","42")` is always `users:alice:42`, `String()`
is always the bare namespace, and an empty segment survives as an empty field. The
subtle property is the no-mutation guarantee — `join` builds a fresh, pre-sized
slice and copies the segments in, so a caller who splats a slice keeps its data
intact. The mistake to avoid is "optimizing" `join` to append onto `parts`
directly; that saves one allocation and introduces the classic caller-corruption
bug. Run `go test -race` to confirm the whole thing under the race detector.

## Resources

- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)
- [`strings.Join`](https://pkg.go.dev/strings#Join)
- [Effective Go: Variadic functions](https://go.dev/doc/effective_go#variadic-functions)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-printf-style-arg-forwarding.md](02-printf-style-arg-forwarding.md)
