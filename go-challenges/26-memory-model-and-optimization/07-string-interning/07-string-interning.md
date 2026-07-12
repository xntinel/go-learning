# 7. String Interning

String interning trades lookup work for memory reuse by returning one canonical string for repeated values. This lesson builds a bounded, thread-safe interner for low-cardinality record fields, with tests that verify validation, concurrency safety under the race detector, and canonical behavior.

```text
internpool/
  go.mod
  intern.go
  intern_test.go
  cmd/demo/main.go
```

## Concepts

### A String Value Is A Header

A Go string value contains a pointer to immutable bytes and a length. Equal strings may or may not share the same backing bytes. Parsing data from files, JSON, CSV, or network buffers can create many separate allocations for values that are logically identical.

### Interning Creates Canonical Values

An interner stores the first copy of a string and returns that same canonical value for future equal strings. This helps when total rows are large and the number of unique values is small, such as status codes, country codes, or known event names.

### Cloning Avoids Retaining Large Buffers

If you intern a small substring that points into a much larger buffer, keeping the substring can keep the larger allocation alive. `strings.Clone` creates an independent copy. Use it deliberately; cloning every string without profiling can make memory use worse.

### Intern Pools Need A Lifecycle

An unbounded interner is a memory leak for high-cardinality data. A production design needs a limit, eviction, sharding, request scoping, or a reset policy. This lesson uses a maximum unique count so the failure mode is explicit and testable.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/26-memory-model-and-optimization/07-string-interning/07-string-interning/cmd/demo
cd go-solutions/26-memory-model-and-optimization/07-string-interning/07-string-interning
```

This is a library package. The demo imports the package and uses only exported API.

### Exercise 1: Implement A Bounded Interner

Create `intern.go`:

```go
package internpool

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	ErrInvalidCapacity = errors.New("capacity must be positive")
	ErrEmptyValue      = errors.New("value must not be empty")
	ErrPoolFull        = errors.New("intern pool is full")
)

type Interner struct {
	mu       sync.RWMutex
	maxItems int
	pool     map[string]string
}

type Record struct {
	Country string
	Status  string
	City    string
}

func New(maxItems int) (*Interner, error) {
	if maxItems <= 0 {
		return nil, fmt.Errorf("new interner: %w: got %d", ErrInvalidCapacity, maxItems)
	}
	return &Interner{
		maxItems: maxItems,
		pool:     make(map[string]string, maxItems),
	}, nil
}

func (in *Interner) Intern(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("intern: %w", ErrEmptyValue)
	}

	in.mu.RLock()
	canonical, ok := in.pool[value]
	in.mu.RUnlock()
	if ok {
		return canonical, nil
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	if canonical, ok := in.pool[value]; ok {
		return canonical, nil
	}
	if len(in.pool) >= in.maxItems {
		return "", fmt.Errorf("intern %q: %w", value, ErrPoolFull)
	}

	canonical = strings.Clone(value)
	in.pool[canonical] = canonical
	return canonical, nil
}

func (in *Interner) Len() int {
	in.mu.RLock()
	defer in.mu.RUnlock()
	return len(in.pool)
}

func InternRecords(in *Interner, records []Record) ([]Record, error) {
	if records == nil {
		return nil, nil
	}
	out := make([]Record, len(records))
	for i, record := range records {
		country, err := in.Intern(record.Country)
		if err != nil {
			return nil, fmt.Errorf("record %d country: %w", i, err)
		}
		status, err := in.Intern(record.Status)
		if err != nil {
			return nil, fmt.Errorf("record %d status: %w", i, err)
		}
		city, err := in.Intern(record.City)
		if err != nil {
			return nil, fmt.Errorf("record %d city: %w", i, err)
		}
		out[i] = Record{Country: country, Status: status, City: city}
	}
	return out, nil
}
```

The `RWMutex` protects the map. The read path returns an existing canonical value without taking the write lock; the write path double-checks after acquiring the write lock.

### Exercise 2: Test Canonicalization And Validation

Create `intern_test.go`:

```go
package internpool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"unsafe"
)

func TestInternReturnsCanonicalString(t *testing.T) {
	t.Parallel()

	in, err := New(4)
	if err != nil {
		t.Fatal(err)
	}

	first, err := in.Intern(string([]byte("active")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := in.Intern(string([]byte("active")))
	if err != nil {
		t.Fatal(err)
	}

	if first != second {
		t.Fatalf("values differ: %q vs %q", first, second)
	}
	if unsafe.StringData(first) != unsafe.StringData(second) {
		t.Fatal("interned equal strings should share canonical backing data")
	}
	if got := in.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
}

func TestInternRecords(t *testing.T) {
	t.Parallel()

	in, err := New(10)
	if err != nil {
		t.Fatal(err)
	}
	records := []Record{
		{Country: "US", Status: "active", City: "Austin"},
		{Country: "US", Status: "active", City: "Austin"},
	}

	out, err := InternRecords(in, records)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || in.Len() != 3 {
		t.Fatalf("len(out)=%d unique=%d", len(out), in.Len())
	}
	if unsafe.StringData(out[0].Country) != unsafe.StringData(out[1].Country) {
		t.Fatal("countries should share canonical data")
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fn   func() error
		want error
	}{
		{name: "invalid capacity", fn: func() error { _, err := New(0); return err }, want: ErrInvalidCapacity},
		{name: "empty value", fn: func() error { in, _ := New(1); _, err := in.Intern(""); return err }, want: ErrEmptyValue},
		{name: "pool full", fn: func() error { in, _ := New(1); _, _ = in.Intern("a"); _, err := in.Intern("b"); return err }, want: ErrPoolFull},
		{name: "record wraps field", fn: func() error {
			in, _ := New(3)
			_, err := InternRecords(in, []Record{{Country: "US", Status: "", City: "Austin"}})
			return err
		}, want: ErrEmptyValue},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := tt.fn(); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestConcurrentIntern(t *testing.T) {
	t.Parallel()

	in, err := New(8)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if _, err := in.Intern("active"); err != nil {
					t.Errorf("Intern() error = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := in.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
}

func ExampleInterner_Intern() {
	in, _ := New(4)
	value, _ := in.Intern("active")
	fmt.Println(value, in.Len())
	// Output: active 1
}
```

The pointer comparison uses `unsafe.StringData` only inside tests to prove the canonical backing data property. The package API itself remains ordinary strings.

### Exercise 3: Add The Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"internpool"
)

func main() {
	in, err := internpool.New(16)
	if err != nil {
		log.Fatal(err)
	}

	records := []internpool.Record{
		{Country: "US", Status: "active", City: "Austin"},
		{Country: "US", Status: "active", City: "Austin"},
		{Country: "DE", Status: "inactive", City: "Berlin"},
	}
	processed, err := internpool.InternRecords(in, records)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("records=%d unique=%d first=%s/%s\n", len(processed), in.Len(), processed[0].Country, processed[0].Status)
}
```

## Common Mistakes

### Interning High-Cardinality Data Forever

Wrong: using one global unbounded interner for user IDs, request IDs, or arbitrary paths.

Fix: bound the pool, scope it to a batch, or use an eviction policy. This lesson returns `ErrPoolFull` when the configured maximum is reached.

### Keeping Substrings Of Large Buffers

Wrong: storing parsed substrings directly when they may point into a large input buffer.

Fix: clone the value before storing it as canonical. The lesson uses `strings.Clone` at the point where a new value enters the pool.

### Assuming Equal Strings Share Memory

Wrong: relying on `a == b` to mean both strings use the same backing bytes.

Fix: equality compares contents. Interning is the explicit mechanism that returns canonical values.

## Verification

Run this from `~/go-exercises/internpool`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test that interns three records with two repeated cities and proves the unique count is the number of distinct field values.

## Summary

- String interning returns one canonical string for repeated equal values.
- `strings.Clone` is useful when retaining a small value that may otherwise keep a large buffer alive.
- A thread-safe interner needs synchronization around its map.
- An interner must have a lifecycle or bound to avoid unbounded memory growth.

## What's Next

Next: [sync.Pool Tuning](../08-sync-pool-tuning/08-sync-pool-tuning.md).

## Resources

- [strings.Clone](https://pkg.go.dev/strings#Clone)
- [unsafe.StringData](https://pkg.go.dev/unsafe#StringData)
- [unique package](https://pkg.go.dev/unique)
- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)
