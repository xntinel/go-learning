# 7. Reflection Performance Costs

Reflection is not free. Every `reflect.ValueOf` call boxes the value into an interface, every `FieldByName` scans field names linearly, and every `Call` allocates a `[]reflect.Value` slice for arguments and results. On hot paths that run millions of times per second, these costs compound. On cold paths that run once at startup, they are irrelevant. The skill is knowing the difference — and knowing how to reduce the cost when it matters.

```text
reflperf/
  go.mod
  bench_test.go
  cache.go
  cache_test.go
  cmd/demo/main.go
```

## Concepts

### Where the Cost Lives

Three operations dominate reflection overhead:

1. **`reflect.ValueOf(x)`** — allocates when the interface value escapes to the heap. For a small value that is only used locally, the compiler may avoid the allocation, but this is not guaranteed and changes across Go versions.

2. **`FieldByName(name string)`** — performs a linear scan of the struct's field names on every call. A struct with 20 fields means up to 20 string comparisons per lookup. Index-based access via `Field(i)` avoids the scan entirely.

3. **`method.Call(args []reflect.Value)`** — must validate argument types, allocate a `[]reflect.Value` for the results, and go through runtime type-checking. A direct method call compiles to a single CALL instruction.

### Caching Eliminates Repeated Lookups

`reflect.Type` is a singleton: `reflect.TypeOf(User{})` always returns the same `reflect.Type` pointer for the same type. It is safe to use as a map key. A field-index cache stores the mapping `reflect.Type → map[fieldName]int`, computed once per type and reused on every subsequent call. After the first call for a given type, `FieldByName` is never called again.

### Interface-Based Alternatives

When you control the types, a method (`GetField(name string) any`) dispatch table is faster than reflection because the type switch compiles to a jump table. The cost is that every new field requires updating the method. Reflection is flexible but slower; interface-based dispatch is rigid but fast.

### When Reflection Is Acceptable

Reflection overhead is acceptable — and often the right choice — when:

- The code runs once at startup or when a new type is first encountered (config loading, ORM schema parsing, plugin registration).
- The cost is paid per struct type, not per value, and the type count is small.
- Amortized over the application lifetime, the reflection cost is a rounding error.

Reflection is not acceptable on the hot path when:

- The code runs once per HTTP request, once per database row, or inside a tight loop over millions of values.
- Profiling shows that reflection accounts for a measurable share of wall time.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/27-reflection/07-reflection-performance-costs/07-reflection-performance-costs/cmd/demo
cd go-solutions/27-reflection/07-reflection-performance-costs/07-reflection-performance-costs
```

### Exercise 1: Field-Index Cache

Create `cache.go`:

```go
package reflperf

import (
	"fmt"
	"reflect"
	"sync"
)

// FieldCache maps (reflect.Type, fieldName) → field index, computed once per type.
// It is safe for concurrent use.
type FieldCache struct {
	mu    sync.RWMutex
	store map[reflect.Type]map[string]int
}

// NewFieldCache returns an empty FieldCache.
func NewFieldCache() *FieldCache {
	return &FieldCache{store: make(map[reflect.Type]map[string]int)}
}

// Index returns the field index for fieldName in type t.
// Returns -1 and false if the field does not exist.
// The index map for t is built on first access and cached for all subsequent calls.
func (c *FieldCache) Index(t reflect.Type, fieldName string) (int, bool) {
	c.mu.RLock()
	m, ok := c.store[t]
	c.mu.RUnlock()
	if !ok {
		m = c.build(t)
	}
	idx, found := m[fieldName]
	return idx, found
}

func (c *FieldCache) build(t reflect.Type) map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Double-checked: another goroutine may have built it while we waited.
	if m, ok := c.store[t]; ok {
		return m
	}
	m := make(map[string]int, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		m[t.Field(i).Name] = i
	}
	c.store[t] = m
	return m
}

// StringField returns the string value of fieldName in the struct pointed to by v.
// v must be a pointer to a struct. Returns an error if the field is not found or
// is not a string.
func (c *FieldCache) StringField(v any, fieldName string) (string, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return "", fmt.Errorf("reflperf: expected struct, got %v", rv.Kind())
	}
	idx, ok := c.Index(rv.Type(), fieldName)
	if !ok {
		return "", fmt.Errorf("reflperf: field %q not found in %v", fieldName, rv.Type())
	}
	f := rv.Field(idx)
	if f.Kind() != reflect.String {
		return "", fmt.Errorf("reflperf: field %q is %v, not string", fieldName, f.Kind())
	}
	return f.String(), nil
}
```

### Exercise 2: Benchmarks and Cache Tests

Create `bench_test.go`:

```go
package reflperf

import (
	"reflect"
	"testing"
)

type User struct {
	ID    int
	Name  string
	Email string
	Age   int
	Admin bool
}

var sample = User{ID: 1, Name: "Alice", Email: "alice@example.com", Age: 30, Admin: true}

// BenchmarkDirectField is the baseline: no reflection.
func BenchmarkDirectField(b *testing.B) {
	u := sample
	var s string
	for i := 0; i < b.N; i++ {
		s = u.Name
	}
	_ = s
}

// BenchmarkFieldByName pays the string-scan cost on every iteration.
func BenchmarkFieldByName(b *testing.B) {
	u := sample
	var s string
	for i := 0; i < b.N; i++ {
		s = reflect.ValueOf(u).FieldByName("Name").String()
	}
	_ = s
}

// BenchmarkFieldByIndex uses a pre-computed index to skip the scan.
func BenchmarkFieldByIndex(b *testing.B) {
	u := sample
	const nameIdx = 1 // pre-computed
	var s string
	for i := 0; i < b.N; i++ {
		s = reflect.ValueOf(u).Field(nameIdx).String()
	}
	_ = s
}

// BenchmarkCachedFieldAccess uses FieldCache to avoid both the scan and
// repeated TypeOf calls.
var globalCache = NewFieldCache()

func BenchmarkCachedFieldAccess(b *testing.B) {
	u := sample
	t := reflect.TypeOf(u)
	var s string
	for i := 0; i < b.N; i++ {
		idx, _ := globalCache.Index(t, "Name")
		s = reflect.ValueOf(u).Field(idx).String()
	}
	_ = s
}

// BenchmarkInterfaceSwitch shows the cost of an interface-based dispatch table.
func BenchmarkInterfaceSwitch(b *testing.B) {
	u := sample
	var s string
	for i := 0; i < b.N; i++ {
		s = getUserField(u, "Name")
	}
	_ = s
}

func getUserField(u User, name string) string {
	switch name {
	case "Name":
		return u.Name
	case "Email":
		return u.Email
	default:
		return ""
	}
}
```

Create `cache_test.go`:

```go
package reflperf

import (
	"fmt"
	"testing"
)

func TestFieldCacheStringField(t *testing.T) {
	t.Parallel()

	c := NewFieldCache()
	u := &User{Name: "Bob"}

	got, err := c.StringField(u, "Name")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Bob" {
		t.Fatalf("StringField Name = %q, want Bob", got)
	}
}

func TestFieldCacheMissingField(t *testing.T) {
	t.Parallel()

	c := NewFieldCache()
	u := &User{Name: "Bob"}
	_, err := c.StringField(u, "NoSuchField")
	if err == nil {
		t.Fatal("expected error for missing field")
	}
}

func TestFieldCacheNonStringField(t *testing.T) {
	t.Parallel()

	c := NewFieldCache()
	u := &User{ID: 42}
	_, err := c.StringField(u, "ID")
	if err == nil {
		t.Fatal("expected error for non-string field")
	}
}

func TestFieldCacheSameTypeReturnsSameIndex(t *testing.T) {
	t.Parallel()

	c := NewFieldCache()
	u1 := &User{Name: "Alice"}
	u2 := &User{Name: "Bob"}

	idx1, _ := c.StringField(u1, "Name")
	idx2, _ := c.StringField(u2, "Name")

	// The cache is used on both calls; values should be correct.
	if idx1 != "Alice" || idx2 != "Bob" {
		t.Fatalf("cache returned wrong values: %q, %q", idx1, idx2)
	}
}

func TestFieldCacheConcurrentAccess(t *testing.T) {
	t.Parallel()

	c := NewFieldCache()
	u := &User{Name: "concurrent"}
	const goroutines = 20

	done := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				c.StringField(u, "Name")
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
}

func ExampleFieldCache_StringField() {
	c := NewFieldCache()
	u := &User{Name: "demo"}
	s, _ := c.StringField(u, "Name")
	fmt.Println(s)
	// Output: demo
}
```

Add `"fmt"` to `cache_test.go`'s import block — the Example uses it.

**Your turn:** add `BenchmarkCachedVsFieldByName` that runs both approaches for the same field and uses `b.ReportAllocs()` to show the allocation count difference.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"reflect"

	"example.com/reflperf"
)

func main() {
	type Server struct {
		Host string
		Port int
		TLS  bool
	}

	c := reflperf.NewFieldCache()

	srv := &Server{Host: "localhost", Port: 8080, TLS: true}

	// Warm the cache on the first call.
	host, err := c.StringField(srv, "Host")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("host:", host)

	// Subsequent calls use the cached index.
	host2, _ := c.StringField(srv, "Host")
	fmt.Println("host (cached):", host2)

	// Show type information.
	t := reflect.TypeOf(*srv)
	fmt.Printf("type %v has %d fields\n", t, t.NumField())
}
```

Run with `go run ./cmd/demo`.

## Common Mistakes

### Using FieldByName Inside a Loop

Wrong: calling `v.FieldByName("Name")` on every iteration of a loop that processes many structs.

What happens: each call scans all field names linearly. For a struct with 15 fields and 1 million iterations, that is 15 million string comparisons.

Fix: look up the field index once with `reflect.TypeOf(T{}).FieldByName("Name")` (or store it in a `FieldCache`) and use `v.Field(idx)` inside the loop.

### Not Checking CanInterface on Unexported Fields

Wrong: calling `v.Field(i).Interface()` on an unexported field.

What happens: panic — "reflect.Value.Interface: cannot return value obtained from unexported field or method".

Fix: check `reflect.StructField.IsExported()` before calling `Interface()`.

### Assuming reflect.ValueOf Does Not Allocate

Wrong: believing that `reflect.ValueOf(x)` is always free.

What happens: when the value escapes (e.g., stored in an interface, returned from a function), the runtime allocates on the heap. Benchmarks with `-benchmem` reveal this.

Fix: minimize the number of `ValueOf` calls per hot path; reuse `reflect.Value` when the same struct is processed multiple times.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go test -bench=. -benchmem ./...
```

The last command shows benchmark results. Direct field access is orders of magnitude faster than `FieldByName`; the cache closes much of that gap.

## Summary

- `FieldByName` scans field names on every call; use `Field(i)` with a pre-computed index to eliminate the scan.
- Cache `reflect.Type` → `map[fieldName]int` in a `sync.RWMutex`-protected map to pay the scan cost only once per type.
- `reflect.ValueOf` can allocate; count allocations with `b.ReportAllocs()` in benchmarks.
- Reflection is appropriate for cold paths (startup, config, schema parsing); prefer interfaces or code generation on hot paths.
- The `FieldCache` pattern in this lesson is the same pattern used by GORM, `sqlx`, and most production Go ORMs.

## What's Next

Next: [Building a Simple ORM](../08-building-a-simple-orm/08-building-a-simple-orm.md).

## Resources

- [reflect package](https://pkg.go.dev/reflect)
- [Go testing benchmarks](https://pkg.go.dev/testing#hdr-Benchmarks)
- [Profiling Go programs](https://go.dev/blog/pprof)
- [Laws of Reflection](https://go.dev/blog/laws-of-reflection)
