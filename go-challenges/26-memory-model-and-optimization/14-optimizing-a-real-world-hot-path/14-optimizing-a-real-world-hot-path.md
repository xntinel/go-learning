# 14. Optimizing a Real-World Hot Path

Real optimization starts with a contract: keep behavior stable, measure the expensive path, change one thing at a time, and stop when the extra complexity is no longer worth the gain. This lesson builds an HTTP-style request gate that validates headers, checks a token, rate-limits by route, and emits a compact log record without using `encoding/json` on the hot path.

```text
hotgate/
  go.mod
  gate.go
  gate_test.go
  cmd/demo/main.go
```

The package exposes a `Processor` with a caller-owned output buffer. Tests pin validation, rate limiting, and JSON shape so later optimizations cannot silently change behavior.

## Concepts

### Optimize The Contract, Not Just The Loop

A hot path usually has observable behavior: validation errors, output format, rate-limit decisions, and concurrency safety. Optimizing only `ns/op` is not enough. The tests below define the contract first, then the implementation avoids unnecessary work inside that contract.

### Remove Work Before Reusing Work

The fastest allocation is the one that never happens. The implementation avoids map iteration, `fmt.Sprintf`, and `json.Marshal` in the request path. It reads known headers directly, builds keys with a reusable byte buffer, and appends JSON with `strconv.AppendQuote`.

### Be Honest About Shared State

The rate limiter owns a mutex-protected map. That map is shared state and must remain safe under concurrent calls. The processor's scratch buffers are not shared across goroutines; callers provide an output buffer per call, which keeps reuse explicit and avoids hidden races.

### Optimization Stops At The Readability Boundary

Manual JSON construction is justified here because the schema is tiny, fixed, and tested with `json.Unmarshal`. For larger or evolving payloads, `encoding/json` may be the better trade-off even if it allocates more. Optimization should be a local, measured exception, not the default style of the whole codebase.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/hotgate/cmd/demo
cd ~/go-exercises/hotgate
go mod init hotgate
```

This is a library. The command under `cmd/demo` is only a consumer of the exported API.

### Exercise 1: Implement The Hot Path

Create `gate.go`:

```go
package hotgate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

var (
	ErrMissingHeader = errors.New("required header is missing")
	ErrInvalidToken  = errors.New("authorization token is invalid")
	ErrRateLimited   = errors.New("request is rate limited")
)

type Request struct {
	Method        string
	Path          string
	Authorization string
}

type RateLimiter struct {
	mu     sync.Mutex
	limit  int
	counts map[string]int
}

func NewRateLimiter(limit int) (*RateLimiter, error) {
	if limit < 1 {
		return nil, fmt.Errorf("new rate limiter: %w: got %d", ErrRateLimited, limit)
	}
	return &RateLimiter{limit: limit, counts: make(map[string]int)}, nil
}

func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.counts[key]++
	return rl.counts[key] <= rl.limit
}

func (rl *RateLimiter) Reset() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	for key := range rl.counts {
		delete(rl.counts, key)
	}
}

type Processor struct {
	limiter *RateLimiter
	now     func() time.Time
}

func NewProcessor(limiter *RateLimiter) (*Processor, error) {
	if limiter == nil {
		return nil, fmt.Errorf("new processor: %w", ErrRateLimited)
	}
	return &Processor{limiter: limiter, now: time.Now}, nil
}

func (p *Processor) Process(dst []byte, req Request) ([]byte, error) {
	if req.Method == "" {
		return dst, fmt.Errorf("process request: method: %w", ErrMissingHeader)
	}
	if req.Path == "" {
		return dst, fmt.Errorf("process request: path: %w", ErrMissingHeader)
	}
	if req.Authorization == "" {
		return dst, fmt.Errorf("process request: authorization: %w", ErrMissingHeader)
	}
	if len(req.Authorization) < len("Bearer ") || req.Authorization[:len("Bearer ")] != "Bearer " {
		return dst, fmt.Errorf("process request: %w", ErrInvalidToken)
	}

	start := p.now()
	tokenHash := sha256.Sum256([]byte(req.Authorization))
	var tokenHex [sha256.Size * 2]byte
	hex.Encode(tokenHex[:], tokenHash[:])
	tokenPrefix := string(tokenHex[:16])

	rateKey := req.Method + ":" + req.Path + ":" + tokenPrefix
	if !p.limiter.Allow(rateKey) {
		return dst, fmt.Errorf("process request: %w", ErrRateLimited)
	}

	dst = dst[:0]
	dst = append(dst, '{')
	dst = appendJSONString(dst, "timestamp", start.UTC().Format(time.RFC3339Nano))
	dst = append(dst, ',')
	dst = appendJSONString(dst, "method", req.Method)
	dst = append(dst, ',')
	dst = appendJSONString(dst, "path", req.Path)
	dst = append(dst, ',')
	dst = appendJSONString(dst, "token", tokenPrefix)
	dst = append(dst, ',')
	dst = appendJSONString(dst, "rate_key", rateKey)
	dst = append(dst, '}')
	return dst, nil
}

func appendJSONString(dst []byte, key, value string) []byte {
	dst = strconv.AppendQuote(dst, key)
	dst = append(dst, ':')
	dst = strconv.AppendQuote(dst, value)
	return dst
}
```

The implementation still uses clear exported types. The hot path avoids generic formatting and reflection-based JSON encoding, while validation errors remain stable through sentinel errors.

### Exercise 2: Test Correctness Before Benchmarking

Create `gate_test.go`:

```go
package hotgate

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestProcessBuildsStableLogRecord(t *testing.T) {
	t.Parallel()

	processor := newTestProcessor(t, 10)
	out, err := processor.Process(make([]byte, 0, 256), Request{
		Method:        "GET",
		Path:          "/v1/users",
		Authorization: "Bearer abc123",
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if got["timestamp"] != "2026-06-21T12:00:00Z" || got["method"] != "GET" || got["path"] != "/v1/users" {
		t.Fatalf("record = %#v", got)
	}
	if got["token"] == "" || got["rate_key"] == "" {
		t.Fatalf("token and rate_key should be populated: %#v", got)
	}
}

func TestProcessValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  Request
		want error
	}{
		{name: "missing method", req: Request{Path: "/", Authorization: "Bearer abc"}, want: ErrMissingHeader},
		{name: "missing path", req: Request{Method: "GET", Authorization: "Bearer abc"}, want: ErrMissingHeader},
		{name: "missing authorization", req: Request{Method: "GET", Path: "/"}, want: ErrMissingHeader},
		{name: "invalid token", req: Request{Method: "GET", Path: "/", Authorization: "abc"}, want: ErrInvalidToken},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			processor := newTestProcessor(t, 10)
			_, err := processor.Process(nil, tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Process() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestProcessRateLimitsByRouteAndToken(t *testing.T) {
	t.Parallel()

	processor := newTestProcessor(t, 1)
	req := Request{Method: "GET", Path: "/v1/users", Authorization: "Bearer abc123"}
	if _, err := processor.Process(nil, req); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.Process(nil, req); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Process() error = %v, want ErrRateLimited", err)
	}
}

func TestProcessReusesOutputBuffer(t *testing.T) {
	t.Parallel()

	processor := newTestProcessor(t, 10)
	buf := make([]byte, 0, 256)
	out, err := processor.Process(buf, Request{Method: "GET", Path: "/v1/users", Authorization: "Bearer abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if cap(out) != cap(buf) {
		t.Fatalf("cap(out) = %d, want %d", cap(out), cap(buf))
	}
}

func BenchmarkProcess(b *testing.B) {
	processor := newBenchmarkProcessor(b, 1000000000)
	req := Request{Method: "GET", Path: "/v1/users", Authorization: "Bearer abc123"}
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := processor.Process(buf, req); err != nil {
			b.Fatal(err)
		}
	}
}

func ExampleProcessor_Process() {
	limiter, _ := NewRateLimiter(10)
	processor, _ := NewProcessor(limiter)
	processor.now = func() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) }
	out, _ := processor.Process(make([]byte, 0, 256), Request{Method: "GET", Path: "/v1/users", Authorization: "Bearer abc123"})
	var record map[string]string
	_ = json.Unmarshal(out, &record)
	fmt.Println(record["method"], record["path"])
	// Output: GET /v1/users
}

func newTestProcessor(t *testing.T, limit int) *Processor {
	t.Helper()

	limiter, err := NewRateLimiter(limit)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := NewProcessor(limiter)
	if err != nil {
		t.Fatal(err)
	}
	processor.now = func() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) }
	return processor
}

func newBenchmarkProcessor(b *testing.B, limit int) *Processor {
	b.Helper()

	limiter, err := NewRateLimiter(limit)
	if err != nil {
		b.Fatal(err)
	}
	processor, err := NewProcessor(limiter)
	if err != nil {
		b.Fatal(err)
	}
	processor.now = func() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) }
	return processor
}
```

The tests use `json.Unmarshal` to verify the generated log rather than comparing raw object text. JSON object member order is not the contract; the fields and values are.

### Exercise 3: Add A Demo That Uses Only Exported API

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"hotgate"
)

func main() {
	limiter, err := hotgate.NewRateLimiter(100)
	if err != nil {
		log.Fatal(err)
	}
	processor, err := hotgate.NewProcessor(limiter)
	if err != nil {
		log.Fatal(err)
	}

	out, err := processor.Process(make([]byte, 0, 256), hotgate.Request{
		Method:        "GET",
		Path:          "/v1/users",
		Authorization: "Bearer abc123",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(out))
}
```

The demo cannot set the unexported clock hook. That is intentional: tests can control time from inside the package, while external callers use the production clock.

## Common Mistakes

### Optimizing Before Pinning Behavior

Wrong: replace JSON encoding or token handling first, then write tests after the output changes.

Fix: write tests for validation, rate limiting, and output fields before changing the implementation. Optimization should preserve the contract.

### Sharing Scratch Buffers Across Requests

Wrong: put a package-level `[]byte` scratch buffer in the processor and reuse it for every request. Concurrent calls race and corrupt output.

Fix: accept a caller-owned `dst []byte`, reset it inside `Process`, and return the appended result.

### Treating Manual JSON As Always Better

Wrong: hand-build every JSON response in a service to avoid reflection. Complex JSON schemas become fragile and hard to maintain.

Fix: reserve manual JSON for tiny, fixed, heavily measured records. Verify the output with `json.Unmarshal`, as this lesson does.

## Verification

Run this from `~/go-exercises/hotgate`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Then run `go test -bench=BenchmarkProcess -benchmem ./...` and record the output before changing the implementation further.

## Summary

- Real hot-path optimization preserves behavior while reducing measured work.
- Direct field access, caller-owned buffers, and append-based encoding remove common allocations.
- Shared state belongs behind synchronization; scratch buffers should not be hidden globals.
- Sentinel errors wrapped with `%w` keep validation behavior testable after context is added.
- Manual JSON is reasonable only for small, fixed schemas backed by tests.

## What's Next

Next: [reflect.TypeOf and reflect.ValueOf](../../27-reflection/01-reflect-typeof-valueof/01-reflect-typeof-valueof.md).

## Resources

- [Go Diagnostics](https://go.dev/doc/diagnostics)
- [testing package: benchmarks](https://pkg.go.dev/testing)
- [encoding/json package](https://pkg.go.dev/encoding/json)
- [strconv.AppendQuote](https://pkg.go.dev/strconv#AppendQuote)
