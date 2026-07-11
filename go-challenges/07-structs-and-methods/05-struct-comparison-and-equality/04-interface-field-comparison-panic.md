# Exercise 4: Cache Change-Detection: The Interface == Runtime Panic

A common optimization: an in-memory cache whose `Set` returns whether the value
actually changed, so the caller can skip a downstream publish when nothing moved.
The obvious implementation compares old and new with `==`. It compiles, passes every
test with string and int values, and then panics in production the first time a
value is a `[]byte`. This exercise reproduces that panic, then fixes it with a
comparability guard.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
changecache/                independent module: example.com/changecache
  go.mod                    go 1.26
  changecache.go            naiveChanged (panics), valuesEqual guard, Cache.Set (safe)
  cmd/
    demo/
      main.go               runnable demo: set comparable and []byte values
  changecache_test.go       fast == path; []byte does not panic; recover proves naive panics
```

- Files: `changecache.go`, `cmd/demo/main.go`, `changecache_test.go`.
- Implement: `Cache.Set(key string, val any) bool` returning whether the value changed, using a `reflect.Type.Comparable` guard with a `reflect.DeepEqual`/`bytes.Equal` fallback.
- Test: comparable values take the `==` path; a `[]byte` value must not panic and detects change correctly; a `recover`-based test proves the naive `==` version panics.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/changecache/cmd/demo
cd ~/go-exercises/changecache
go mod init example.com/changecache
```

### Why `oldVal == newVal` is a landmine on `any`

The cache stores `any`, so `oldVal` and `newVal` are interface values. Comparing two
interface values with `==` is a *dynamic* operation: at run time it checks that the
dynamic types are identical and then compares the dynamic values with `==`. If that
dynamic type is not comparable — a `[]byte`, a map, a func, or a struct containing
one — the runtime panics with `comparing uncomparable type []uint8`. The comparison
compiles without complaint, because at compile time the operands are just `any`; the
type that will actually be compared is unknown until run time. That is what makes it
a latent crash: your tests with `string`/`int` values are green, and the first
cached `[]byte` payload takes the whole request down.

The fix is a comparability guard. Before using `==`, ask reflection whether the
dynamic type is comparable: `reflect.TypeOf(v).Comparable()`. If both values are
comparable and of the same type, the fast `==` path is safe. Otherwise fall back to
a comparison that works on incomparable data — `bytes.Equal` for the common
`[]byte` case, `reflect.DeepEqual` for the general case. `valuesEqual` below encodes
that decision tree, and `Set` uses it so `Set` never panics regardless of what the
caller stores.

Both functions live in the same file so the exercise can *test that the naive one
panics*, proving the guard is load-bearing and not decoration. `naiveChanged` is
real, assembled code — it compiles fine; the defect is purely at run time, which is
exactly why it is dangerous.

Create `changecache.go`:

```go
package changecache

import (
	"bytes"
	"reflect"
)

// naiveChanged is the tempting, WRONG implementation: it compares two interface
// values with ==. It compiles, but panics at run time when the dynamic type is
// not comparable (e.g. []byte). Kept here so a test can prove it panics.
func naiveChanged(oldVal, newVal any) bool {
	return oldVal != newVal
}

// valuesEqual reports whether two interface values are equal without ever
// panicking. It uses the fast == path only when both dynamic types are the same
// and comparable, and falls back to bytes.Equal / reflect.DeepEqual otherwise.
func valuesEqual(a, b any) bool {
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb {
		return false
	}
	if ta == nil { // both are untyped nil
		return true
	}
	// Common incomparable case worth special-casing for correctness and speed.
	if ba, ok := a.([]byte); ok {
		return bytes.Equal(ba, b.([]byte))
	}
	if ta.Comparable() {
		return a == b // safe: same comparable dynamic type
	}
	return reflect.DeepEqual(a, b)
}

// Cache is an in-memory cache whose Set reports whether the value changed, so a
// caller can skip a downstream publish on a no-op write.
type Cache struct {
	items map[string]any
}

// New returns an empty Cache.
func New() *Cache { return &Cache{items: make(map[string]any)} }

// Set stores val under key and reports whether it differs from the previous
// value (a brand-new key counts as changed). Set never panics on incomparable
// values because it compares through valuesEqual, not raw ==.
func (c *Cache) Set(key string, val any) (changed bool) {
	old, existed := c.items[key]
	c.items[key] = val
	if !existed {
		return true
	}
	return !valuesEqual(old, val)
}
```

### The runnable demo

The demo sets a comparable string value twice (unchanged the second time), changes
it, then sets a `[]byte` value twice — the case that would crash the naive version —
and shows the guarded `Set` detects change correctly without panicking.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/changecache"
)

func main() {
	c := changecache.New()

	fmt.Printf("new key: %v\n", c.Set("flag", "on"))
	fmt.Printf("same value: %v\n", c.Set("flag", "on"))
	fmt.Printf("changed value: %v\n", c.Set("flag", "off"))

	// []byte is not comparable: naive == would panic here.
	fmt.Printf("new blob: %v\n", c.Set("blob", []byte("abc")))
	fmt.Printf("same blob: %v\n", c.Set("blob", []byte("abc")))
	fmt.Printf("changed blob: %v\n", c.Set("blob", []byte("abd")))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
new key: true
same value: false
changed value: true
new blob: true
same blob: false
changed blob: true
```

### Tests

`TestComparableFastPath` checks that comparable values (`string`, `int`) go through
`Set` and detect change correctly. `TestByteSliceDoesNotPanic` is the core case:
storing and re-storing `[]byte` values must not panic and must correctly detect
equal vs changed content — the guard's whole reason to exist.
`TestNaivePanicsOnByteSlice` proves the guard is load-bearing by showing the naive
`==` version *does* panic on `[]byte`, captured with `recover`.

Create `changecache_test.go`:

```go
package changecache

import (
	"testing"
)

func TestComparableFastPath(t *testing.T) {
	t.Parallel()

	c := New()
	if !c.Set("k", "a") {
		t.Fatal("new key should be changed")
	}
	if c.Set("k", "a") {
		t.Fatal("same string should be unchanged")
	}
	if !c.Set("k", "b") {
		t.Fatal("different string should be changed")
	}
	if !c.Set("n", 1) {
		t.Fatal("new int key should be changed")
	}
	if c.Set("n", 1) {
		t.Fatal("same int should be unchanged")
	}
}

func TestByteSliceDoesNotPanic(t *testing.T) {
	t.Parallel()

	c := New()
	if !c.Set("b", []byte("abc")) {
		t.Fatal("new blob should be changed")
	}
	if c.Set("b", []byte("abc")) {
		t.Fatal("identical blob content should be unchanged")
	}
	if !c.Set("b", []byte("abd")) {
		t.Fatal("different blob content should be changed")
	}
}

func TestNaivePanicsOnByteSlice(t *testing.T) {
	t.Parallel()

	panicked := func() (p bool) {
		defer func() {
			if r := recover(); r != nil {
				p = true
			}
		}()
		_ = naiveChanged([]byte("a"), []byte("a"))
		return false
	}()

	if !panicked {
		t.Fatal("naiveChanged must panic on []byte; the guard in Set is load-bearing")
	}
}

func TestNaiveIsFineForComparable(t *testing.T) {
	t.Parallel()

	if naiveChanged("a", "a") {
		t.Fatal("naiveChanged should report equal strings as unchanged")
	}
	if !naiveChanged("a", "b") {
		t.Fatal("naiveChanged should report different strings as changed")
	}
}
```

## Review

The cache is correct when `Set` never panics and reports change accurately for both
comparable and incomparable value types. The load-bearing test is
`TestNaivePanicsOnByteSlice`: it demonstrates the exact production failure the guard
prevents, so if someone "simplifies" `valuesEqual` back to a raw `==`, that test
turns red instead of a service turning over. Note the two design details that keep
`valuesEqual` correct: it first checks the dynamic types are identical (so an `int`
and an `int64` are never accidentally equal), and it special-cases `[]byte` with
`bytes.Equal` both because it is the most common incomparable payload and because it
is faster than routing through `reflect.DeepEqual`. Run `go test -race`.

## Resources

- [Go spec: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — the rule that interface comparison panics on an incomparable dynamic type.
- [reflect.Type.Comparable](https://pkg.go.dev/reflect#Type) — the runtime comparability check used to guard `==`.
- [bytes.Equal](https://pkg.go.dev/bytes#Equal) — the fast, allocation-free comparison for the `[]byte` fallback.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-time-equal-monotonic-location.md](05-time-equal-monotonic-location.md)
