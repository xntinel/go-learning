# 8. sync.Pool Tuning

`sync.Pool` is for temporary objects shared across independent callers, not for durable caches. This lesson builds a JSON response encoder that reuses buffers internally while preserving a safe exported API that returns owned bytes.

```text
pooljson/
  go.mod
  processor.go
  processor_test.go
  cmd/demo/main.go
```

## Concepts

### A Pool Is Best-Effort Temporary Reuse

`sync.Pool` may drop stored items at any time. A `Get` call can ignore previously stored objects and call `New` instead. That behavior is why a pool is useful for reducing allocation pressure but unsuitable as a cache or resource ownership mechanism.

### Reset Before Use, Copy Before Put

Pooled objects arrive with unknown previous contents. A buffer must be reset before writing. If the function returns bytes to the caller, it must copy the bytes before putting the buffer back, because the pooled buffer can be reused immediately after `Put`.

### Pool Pointer Types

The `sync.Pool` documentation recommends pointer types for pooled values because they avoid extra interface allocation and represent reusable mutable state clearly. This lesson pools `*bytes.Buffer` and keeps the pool unexported.

### Measure The Hot Path

Pooling helps when allocation cost is significant and the object is reused many times. It can hurt simple or cold paths by adding synchronization, retention, and complexity. Benchmark with `b.ReportAllocs()` before keeping a pool.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/26-memory-model-and-optimization/08-sync-pool-tuning/08-sync-pool-tuning/cmd/demo
cd go-solutions/26-memory-model-and-optimization/08-sync-pool-tuning/08-sync-pool-tuning
```

This package exposes a processor API. The pool is an implementation detail.

### Exercise 1: Implement The Processor

Create `processor.go`:

```go
package pooljson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrNilRequest      = errors.New("request must not be nil")
	ErrInvalidMaxData  = errors.New("max data bytes must be positive")
	ErrPayloadTooLarge = errors.New("payload is too large")
)

type Request struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Data string `json:"data"`
}

type Response struct {
	ID      int    `json:"id"`
	Message string `json:"message"`
}

type Processor struct {
	maxData int
	pool    sync.Pool
}

func New(maxData int) (*Processor, error) {
	if maxData <= 0 {
		return nil, fmt.Errorf("new processor: %w: got %d", ErrInvalidMaxData, maxData)
	}
	p := &Processor{maxData: maxData}
	p.pool.New = func() any {
		return new(bytes.Buffer)
	}
	return p, nil
}

func (p *Processor) Process(req *Request) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("process: %w", ErrNilRequest)
	}
	if len(req.Data) > p.maxData {
		return nil, fmt.Errorf("process: %w: got %d max %d", ErrPayloadTooLarge, len(req.Data), p.maxData)
	}

	buf := p.pool.Get().(*bytes.Buffer)
	buf.Reset()
	defer p.pool.Put(buf)

	resp := Response{ID: req.ID, Message: "processed: " + req.Name}
	if err := json.NewEncoder(buf).Encode(resp); err != nil {
		return nil, fmt.Errorf("encode response: %w", err)
	}

	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}

func ProcessWithoutPool(req *Request, maxData int) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("process without pool: %w", ErrNilRequest)
	}
	if maxData <= 0 {
		return nil, fmt.Errorf("process without pool: %w: got %d", ErrInvalidMaxData, maxData)
	}
	if len(req.Data) > maxData {
		return nil, fmt.Errorf("process without pool: %w: got %d max %d", ErrPayloadTooLarge, len(req.Data), maxData)
	}

	var buf bytes.Buffer
	resp := Response{ID: req.ID, Message: "processed: " + req.Name}
	if err := json.NewEncoder(&buf).Encode(resp); err != nil {
		return nil, fmt.Errorf("encode response: %w", err)
	}
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}
```

`Process` returns a copy, not `buf.Bytes()` directly. That copy is required because the buffer goes back to the pool before the caller uses the returned data.

### Exercise 2: Test Safety And Validation

Create `processor_test.go`:

```go
package pooljson

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestProcessMatchesBaseline(t *testing.T) {
	t.Parallel()

	req := &Request{ID: 42, Name: "Ada", Data: "payload"}
	p, err := New(100)
	if err != nil {
		t.Fatal(err)
	}

	pooled, err := p.Process(req)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	baseline, err := ProcessWithoutPool(req, 100)
	if err != nil {
		t.Fatalf("ProcessWithoutPool() error = %v", err)
	}
	if string(pooled) != string(baseline) {
		t.Fatalf("pooled = %s baseline = %s", pooled, baseline)
	}

	var resp Response
	if err := json.Unmarshal(pooled, &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if resp.ID != 42 || resp.Message != "processed: Ada" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestReturnedBytesAreOwned(t *testing.T) {
	t.Parallel()

	p, err := New(100)
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.Process(&Request{ID: 1, Name: "first"})
	if err != nil {
		t.Fatal(err)
	}
	firstCopy := string(first)

	for i := 0; i < 20; i++ {
		if _, err := p.Process(&Request{ID: i + 2, Name: "second"}); err != nil {
			t.Fatal(err)
		}
	}
	if string(first) != firstCopy {
		t.Fatalf("returned bytes changed: got %q want %q", string(first), firstCopy)
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fn   func() error
		want error
	}{
		{name: "new invalid", fn: func() error { _, err := New(0); return err }, want: ErrInvalidMaxData},
		{name: "pooled nil request", fn: func() error { p, _ := New(10); _, err := p.Process(nil); return err }, want: ErrNilRequest},
		{name: "pooled too large", fn: func() error { p, _ := New(3); _, err := p.Process(&Request{Data: "abcd"}); return err }, want: ErrPayloadTooLarge},
		{name: "baseline nil request", fn: func() error { _, err := ProcessWithoutPool(nil, 10); return err }, want: ErrNilRequest},
		{name: "baseline invalid limit", fn: func() error { _, err := ProcessWithoutPool(&Request{}, 0); return err }, want: ErrInvalidMaxData},
		{name: "baseline too large", fn: func() error { _, err := ProcessWithoutPool(&Request{Data: strings.Repeat("x", 4)}, 3); return err }, want: ErrPayloadTooLarge},
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

func TestProcessTable(t *testing.T) {
	t.Parallel()

	p, err := New(100)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		req  *Request
		want string
	}{
		{name: "ada", req: &Request{ID: 1, Name: "Ada"}, want: "processed: Ada"},
		{name: "ken", req: &Request{ID: 2, Name: "Ken"}, want: "processed: Ken"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := p.Process(tt.req)
			if err != nil {
				t.Fatal(err)
			}
			var resp Response
			if err := json.Unmarshal(data, &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Message != tt.want {
				t.Fatalf("Message = %q, want %q", resp.Message, tt.want)
			}
		})
	}
}

func ExampleProcessor_Process() {
	p, _ := New(100)
	data, _ := p.Process(&Request{ID: 7, Name: "Ada"})
	fmt.Print(string(data))
	// Output: {"id":7,"message":"processed: Ada"}
}
```

The race detector is part of verification. It checks that concurrent tests do not expose unsafe sharing of the pooled buffer.

### Exercise 3: Add The Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"pooljson"
)

func main() {
	p, err := pooljson.New(1024)
	if err != nil {
		log.Fatal(err)
	}

	data, err := p.Process(&pooljson.Request{ID: 101, Name: "Ada", Data: "request body"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(string(data))
}
```

Optional benchmark to add after the tests pass:

```go
func BenchmarkProcess(b *testing.B) {
	p, err := New(1024)
	if err != nil {
		b.Fatal(err)
	}
	req := &Request{ID: 1, Name: "Ada", Data: "payload"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := p.Process(req); err != nil {
			b.Fatal(err)
		}
	}
}
```

## Common Mistakes

### Returning Pooled Buffer Memory

Wrong: `return buf.Bytes(), nil` followed by `defer pool.Put(buf)`.

Fix: copy the bytes before returning. The lesson's `TestReturnedBytesAreOwned` proves later pool reuse cannot mutate the caller's result.

### Treating sync.Pool As A Cache

Wrong: storing values in a pool and expecting future `Get` calls to retrieve them.

Fix: use a real cache when retention matters. A pool can drop values whenever the runtime chooses.

### Pooling Cold Or Tiny Allocations Without Measurement

Wrong: adding a pool around every allocation because reuse sounds faster.

Fix: benchmark with `b.ReportAllocs()` and keep the pool only when it reduces allocation pressure on a hot path.

## Verification

Run this from `~/go-exercises/pooljson`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one benchmark comparing `Process` and `ProcessWithoutPool`, then run `go test -bench=. -benchmem` to decide whether the pool is worth keeping for your workload.

## Summary

- `sync.Pool` provides best-effort reuse for temporary objects.
- Reset pooled mutable objects before reuse.
- Copy data out before returning the object to the pool.
- Pools should be justified with allocation measurements, not added by default.

## What's Next

Next: [Trace Tool and Goroutine Scheduling](../09-trace-tool-goroutine-scheduling/09-trace-tool-goroutine-scheduling.md).

## Resources

- [sync.Pool documentation](https://pkg.go.dev/sync#Pool)
- [bytes.Buffer documentation](https://pkg.go.dev/bytes#Buffer)
- [encoding/json Encoder documentation](https://pkg.go.dev/encoding/json#Encoder)
- [Go Blog: Profiling Go Programs](https://go.dev/blog/pprof)
