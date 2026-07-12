# 3. Memory Profiling

Memory profiling shows where a Go program allocates and what remains live. This lesson builds a reusable transformer that can either allocate fresh buffers or reuse scratch space, then tests the validation contract that makes memory experiments safe to run repeatedly.

```text
memprof/
  go.mod
  transform.go
  transform_test.go
  cmd/demo/main.go
```

## Concepts

### Heap Profiles Have Multiple Views

Go heap profiles contain sampled allocation data. `inuse_space` shows live heap bytes after a recent garbage collection. `alloc_space` shows cumulative bytes allocated, including objects that have already been freed. A leak hunt usually starts with in-use data; GC pressure analysis often starts with allocation data.

The same profile can be viewed with different pprof sample indexes, such as `-inuse_space`, `-inuse_objects`, `-alloc_space`, and `-alloc_objects`.

### Allocation Rate Can Matter More Than Retained Size

A function can allocate heavily without retaining much memory. That code may not dominate `inuse_space`, but it can still drive garbage collector work and latency. Benchmarks with `-benchmem` expose bytes per operation and allocations per operation.

The lesson's `Transform` function returns new strings, while `Transformer.TransformInto` reuses a caller-owned slice. Both are correct, but their allocation behavior differs.

### Preallocation Is A Contract

Preallocating capacity helps only when the code keeps using that capacity. A reusable type should document whether it retains scratch storage and whether it is safe for concurrent use.

The `Transformer` type owns scratch storage and is not safe for concurrent use. That is an intentional API choice for a performance-oriented helper.

### Profile Mode Names Should Be Validated

Profiling tools accept specific sample indexes. A wrapper that accepts a string mode should reject unknown modes early. Sentinel validation errors make tests precise and let callers branch with `errors.Is`.

## Exercises

### Exercise 1: Build The Transformer

Create `transform.go`:

```go
package memprof

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	ErrEmptyPrefix = errors.New("prefix must not be empty")
	ErrBadLimit    = errors.New("limit must be positive")
	ErrBadMode     = errors.New("profile mode is not supported")
)

type Options struct {
	Prefix string
	Limit  int
}

type ProfileMode string

const (
	InUseSpace   ProfileMode = "inuse_space"
	InUseObjects ProfileMode = "inuse_objects"
	AllocSpace   ProfileMode = "alloc_space"
	AllocObjects ProfileMode = "alloc_objects"
)

func ValidateMode(mode ProfileMode) error {
	switch mode {
	case InUseSpace, InUseObjects, AllocSpace, AllocObjects:
		return nil
	default:
		return fmt.Errorf("validate profile mode %q: %w", mode, ErrBadMode)
	}
}

func Transform(input []string, opts Options) ([]string, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	limit := min(opts.Limit, len(input))
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, format(opts.Prefix, input[i], i))
	}
	return out, nil
}

type Transformer struct {
	scratch []string
}

func NewTransformer(capacity int) (*Transformer, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("new transformer: %w: got %d", ErrBadLimit, capacity)
	}
	return &Transformer{scratch: make([]string, 0, capacity)}, nil
}

func (t *Transformer) TransformInto(input []string, opts Options) ([]string, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	limit := min(opts.Limit, len(input))
	if cap(t.scratch) < limit {
		t.scratch = make([]string, 0, limit)
	}
	out := t.scratch[:0]
	for i := 0; i < limit; i++ {
		out = append(out, format(opts.Prefix, input[i], i))
	}
	t.scratch = out
	return out, nil
}

func (t *Transformer) Capacity() int {
	return cap(t.scratch)
}

func validateOptions(opts Options) error {
	if strings.TrimSpace(opts.Prefix) == "" {
		return fmt.Errorf("transform options: %w", ErrEmptyPrefix)
	}
	if opts.Limit <= 0 {
		return fmt.Errorf("transform options: %w: got %d", ErrBadLimit, opts.Limit)
	}
	return nil
}

func format(prefix, value string, index int) string {
	return prefix + "-" + strconv.Itoa(index) + "-" + strings.ToUpper(value)
}
```

### Exercise 2: Test Validation, Reuse, And Examples

Create `transform_test.go`:

```go
package memprof

import (
	"errors"
	"fmt"
	"testing"
)

func TestTransformValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		opts    Options
		wantErr error
	}{
		{name: "empty prefix", opts: Options{Prefix: " ", Limit: 1}, wantErr: ErrEmptyPrefix},
		{name: "bad limit", opts: Options{Prefix: "item", Limit: 0}, wantErr: ErrBadLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Transform([]string{"a"}, tc.opts)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mode    ProfileMode
		wantErr error
	}{
		{name: "alloc", mode: AllocSpace},
		{name: "bad", mode: "retained", wantErr: ErrBadMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateMode(tc.mode)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestTransformLimitsOutput(t *testing.T) {
	t.Parallel()

	got, err := Transform([]string{"a", "b", "c"}, Options{Prefix: "row", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[row-0-A row-1-B]" {
		t.Fatalf("got %v", got)
	}
}

func TestTransformerReusesCapacity(t *testing.T) {
	t.Parallel()

	tr, err := NewTransformer(4)
	if err != nil {
		t.Fatal(err)
	}
	first, err := tr.TransformInto([]string{"a", "b"}, Options{Prefix: "row", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := tr.TransformInto([]string{"c"}, Options{Prefix: "row", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Capacity() != 4 {
		t.Fatalf("capacity = %d, want 4", tr.Capacity())
	}
	if len(first) != 2 || len(second) != 1 || second[0] != "row-0-C" {
		t.Fatalf("first=%v second=%v", first, second)
	}
}

func TestNewTransformerRejectsBadCapacity(t *testing.T) {
	t.Parallel()

	if _, err := NewTransformer(0); !errors.Is(err, ErrBadLimit) {
		t.Fatalf("err = %v, want ErrBadLimit", err)
	}
}

func BenchmarkTransform(b *testing.B) {
	input := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	for b.Loop() {
		if _, err := Transform(input, Options{Prefix: "row", Limit: len(input)}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTransformerTransformInto(b *testing.B) {
	input := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	tr, err := NewTransformer(len(input))
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := tr.TransformInto(input, Options{Prefix: "row", Limit: len(input)}); err != nil {
			b.Fatal(err)
		}
	}
}

func ExampleTransform() {
	got, _ := Transform([]string{"alpha", "bravo"}, Options{Prefix: "row", Limit: 2})
	fmt.Println(got)
	// Output: [row-0-ALPHA row-1-BRAVO]
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"memprof"
)

func main() {
	tr, err := memprof.NewTransformer(8)
	if err != nil {
		log.Fatal(err)
	}
	rows, err := tr.TransformInto([]string{"alpha", "bravo", "charlie"}, memprof.Options{Prefix: "row", Limit: 3})
	if err != nil {
		log.Fatal(err)
	}
	if err := memprof.ValidateMode(memprof.AllocSpace); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("rows=%v capacity=%d mode=%s\n", rows, tr.Capacity(), memprof.AllocSpace)
}
```

After the verification gate passes, collect allocation data with:

```bash
go test -bench=. -benchmem -memprofile=mem.prof
go tool pprof -alloc_space mem.prof
```

Use `top` at the pprof prompt, then compare with `go tool pprof -inuse_space mem.prof`.

## Common Mistakes

### Looking Only At Live Heap

Wrong: check only `inuse_space` and conclude allocation-heavy code is cheap.

Fix: inspect `alloc_space` and benchmark `allocs/op` when GC pressure is the concern.

### Reusing Scratch Storage Without Documenting Ownership

Wrong: return internal scratch storage from a concurrent type and let callers share it across goroutines.

Fix: document the ownership model. `Transformer` is intentionally stateful and not safe for concurrent use.

### Treating Mode Strings As Free Text

Wrong: accept any pprof mode string and fail later in a shell command.

Fix: validate modes with `ValidateMode`, and assert `errors.Is(err, ErrBadMode)` for unsupported values.

## Verification

Run this from `~/go-exercises/memprof`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test that calls `ValidateMode(InUseObjects)` and expects no error.

## Summary

- `inuse_space` shows live heap; `alloc_space` shows cumulative allocation volume.
- Allocation-heavy code can hurt latency even when objects are short-lived.
- Benchmarks with `-benchmem` expose `B/op` and `allocs/op`.
- Reuse can reduce allocation rate, but the API must define ownership and concurrency rules.
- Sentinel validation errors make profiling helpers testable.

## What's Next

Next: [Benchmarking Methodology](../04-benchmarking-methodology/04-benchmarking-methodology.md).

## Resources

- [runtime/pprof package](https://pkg.go.dev/runtime/pprof)
- [Profiling Go Programs](https://go.dev/blog/pprof)
- [Diagnostics: Profiling](https://go.dev/doc/diagnostics#profiling)
- [testing package benchmarks](https://pkg.go.dev/testing#hdr-Benchmarks)
