# 2. CPU Profiling with pprof

CPU profiling answers where a Go program spends time while actively running on CPU. This lesson builds a small text-processing library with a deliberate workload, then adds a safe wrapper around `runtime/pprof` so profiling is part of the package contract instead of an ad hoc `main` program.

```text
cpuprof/
  go.mod
  processor.go
  processor_test.go
  cmd/demo/main.go
```

## Concepts

### CPU Profiles Are Samples

Go's CPU profiler samples the running program and records stack traces. The result is statistical, not an exact trace of every call. Longer runs usually produce more stable profiles because they collect more samples.

`go test -bench=. -cpuprofile=cpu.prof` is the lowest-friction way to profile library code. For a standalone program, `runtime/pprof.StartCPUProfile` starts profiling and `runtime/pprof.StopCPUProfile` flushes the profile before exit.

### Flat Time And Cumulative Time Answer Different Questions

The `top` view in `go tool pprof` reports flat time and cumulative time. Flat time is time spent in that function itself. Cumulative time includes callees. A dispatcher can have low flat time but high cumulative time because it calls expensive functions.

The lesson's `Process` function calls string and hashing work. In a profile, the useful question is not only whether `Process` appears, but which child frames account for the cost.

### Benchmark Profiles Are Repeatable

Benchmarks give pprof a controlled workload. They also avoid exposing `net/http/pprof` endpoints in a service just to learn the profiler. HTTP profiling is valuable in production, but it needs access control and a representative load.

The package includes benchmarks for profiling and ordinary tests for correctness. `go test` runs both kinds of code from the same module, but benchmarks run only when requested with `-bench`.

### Profiling Code Still Needs Errors

`StartCPUProfile` returns an error if CPU profiling is already active. A profile writer can also fail. Production profiling helpers must return errors instead of hiding them in logs or panics.

The `CaptureCPU` helper validates its writer and workload, wraps sentinel validation errors with `%w`, starts profiling, runs the workload, and stops profiling with `defer`.

## Exercises

### Exercise 1: Build The Processing Library

Create `processor.go`:

```go
package cpuprof

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime/pprof"
	"strings"
)

var (
	ErrEmptyInput = errors.New("input must not be empty")
	ErrBadRepeat  = errors.New("repeat must be positive")
	ErrNilWriter  = errors.New("profile writer must not be nil")
	ErrNilWork    = errors.New("profile workload must not be nil")
)

type Options struct {
	Repeat int
}

type Result struct {
	Input  string
	Bytes  int
	Digest string
}

func Process(items []string, opts Options) ([]Result, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("process: %w", ErrEmptyInput)
	}
	if opts.Repeat <= 0 {
		return nil, fmt.Errorf("process: %w: got %d", ErrBadRepeat, opts.Repeat)
	}
	results := make([]Result, 0, len(items))
	for _, item := range items {
		upper := strings.ToUpper(item)
		var b strings.Builder
		b.Grow(len(upper) * opts.Repeat)
		for i := 0; i < opts.Repeat; i++ {
			b.WriteString(upper)
		}
		payload := b.String()
		sum := sha256.Sum256([]byte(payload))
		results = append(results, Result{
			Input:  item,
			Bytes:  len(payload),
			Digest: hex.EncodeToString(sum[:]),
		})
	}
	return results, nil
}

func CaptureCPU(w io.Writer, work func() error) error {
	if isNilWriter(w) {
		return fmt.Errorf("capture cpu: %w", ErrNilWriter)
	}
	if work == nil {
		return fmt.Errorf("capture cpu: %w", ErrNilWork)
	}
	if err := pprof.StartCPUProfile(w); err != nil {
		return fmt.Errorf("start cpu profile: %w", err)
	}
	defer pprof.StopCPUProfile()
	if err := work(); err != nil {
		return fmt.Errorf("profile workload: %w", err)
	}
	return nil
}

func isNilWriter(w io.Writer) bool {
	if w == nil {
		return true
	}
	v := reflect.ValueOf(w)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
```

### Exercise 2: Test Validation And Examples

Create `processor_test.go`:

```go
package cpuprof

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestProcessValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		items   []string
		opts    Options
		wantErr error
	}{
		{name: "empty input", items: nil, opts: Options{Repeat: 1}, wantErr: ErrEmptyInput},
		{name: "bad repeat", items: []string{"go"}, opts: Options{Repeat: 0}, wantErr: ErrBadRepeat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Process(tc.items, tc.opts)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestProcessReturnsDeterministicResults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		items     []string
		repeat    int
		wantBytes int
	}{
		{name: "single", items: []string{"go"}, repeat: 3, wantBytes: 6},
		{name: "two", items: []string{"go", "cpu"}, repeat: 2, wantBytes: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Process(tc.items, Options{Repeat: tc.repeat})
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Bytes != tc.wantBytes || got[0].Digest == "" {
				t.Fatalf("first result = %+v", got[0])
			}
		})
	}
}

func TestCaptureCPUValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		writer  *bytes.Buffer
		work    func() error
		wantErr error
	}{
		{name: "nil writer", writer: nil, work: func() error { return nil }, wantErr: ErrNilWriter},
		{name: "nil work", writer: &bytes.Buffer{}, work: nil, wantErr: ErrNilWork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CaptureCPU(tc.writer, tc.work)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestCaptureCPURunsWork(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	called := false
	err := CaptureCPU(&buf, func() error {
		called = true
		_, err := Process([]string{"alpha", "bravo", "charlie"}, Options{Repeat: 100})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("workload was not called")
	}
}

func BenchmarkProcess(b *testing.B) {
	items := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	for b.Loop() {
		if _, err := Process(items, Options{Repeat: 100}); err != nil {
			b.Fatal(err)
		}
	}
}

func ExampleProcess() {
	results, _ := Process([]string{"go"}, Options{Repeat: 3})
	fmt.Printf("%s %d\n", results[0].Input, results[0].Bytes)
	// Output: go 6
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"cpuprof"
)

func main() {
	results, err := cpuprof.Process([]string{"alpha", "bravo"}, cpuprof.Options{Repeat: 10})
	if err != nil {
		log.Fatal(err)
	}
	for _, result := range results {
		fmt.Printf("%s bytes=%d digest=%s\n", result.Input, result.Bytes, result.Digest[:12])
	}
}
```

To collect a CPU profile from the benchmark after the verification gate passes:

```bash
go test -bench=BenchmarkProcess -cpuprofile=cpu.prof -count=3
go tool pprof cpu.prof
```

At the `pprof` prompt, use `top` and `list Process` to inspect the hot paths.

## Common Mistakes

### Profiling Before Correctness Tests

Wrong: collect a profile from code whose behavior is not pinned by tests.

Fix: run the verification gate first. The tests prove that validation and result shape are stable before performance work starts.

### Treating One Short Profile As Truth

Wrong: optimize based on a profile with too few samples.

Fix: run a representative benchmark for long enough to collect useful samples, then confirm with repeated runs.

### Ignoring Profiling Errors

Wrong: call `StartCPUProfile` and ignore its error.

Fix: return the error. `CaptureCPU` wraps validation errors and profiler start errors so callers can diagnose failures.

## Verification

Run this from `~/go-exercises/cpuprof`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test that passes `Options{Repeat: -1}` and asserts `errors.Is(err, ErrBadRepeat)`.

## Summary

- CPU profiles are sampled stack traces, so profile representative workloads.
- Use benchmark profiles for repeatable library measurements.
- `go tool pprof` separates flat cost from cumulative cost.
- `runtime/pprof.StartCPUProfile` must be paired with `StopCPUProfile`.
- Profiling helpers should validate inputs and return wrapped errors.

## What's Next

Next: [Memory Profiling](../03-memory-profiling/03-memory-profiling.md).

## Resources

- [Profiling Go Programs](https://go.dev/blog/pprof)
- [runtime/pprof package](https://pkg.go.dev/runtime/pprof)
- [net/http/pprof package](https://pkg.go.dev/net/http/pprof)
- [Diagnostics: Profiling](https://go.dev/doc/diagnostics#profiling)
