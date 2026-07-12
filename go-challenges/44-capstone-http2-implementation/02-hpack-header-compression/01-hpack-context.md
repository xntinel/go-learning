# Exercise 1: A Connection-Scoped HPACK Context

The canonical `golang.org/x/net/http2/hpack` package gives you an `Encoder` and a `Decoder`, but a real connection needs more: the two must share a synchronized lifetime, sensitive headers must be forced never-indexed regardless of what the caller asks, the decoded header list must be size-bounded against memory exhaustion, and table-size negotiation must flow through one place. This module wraps all of that in an `HPACKContext`.

This module is fully self-contained: it has its own `go mod init`, depends only on `golang.org/x/net/http2/hpack`, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
hpcomp/
  go.mod
  context.go             HPACKContext: Encode, Decode, never-indexed, size limit
  context_test.go        round-trip, sensitive enforcement, size limit, eviction
  cmd/demo/main.go        three sequential request blocks on one connection
```

- Files: `context.go`, `context_test.go`, `cmd/demo/main.go`.
- Implement: `HPACKContext` with `Encode([]hpack.HeaderField) ([]byte, error)`, `Decode([]byte) ([]hpack.HeaderField, error)`, `SetMaxDynamicTableSize`, `MaxDynamicTableSize`, plus the `IsSensitive` predicate.
- Test: a header block round-trips; sensitive headers come back never-indexed; an oversized decoded list is rejected; 20 blocks survive repeated dynamic-table eviction.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/44-capstone-http2-implementation/02-hpack-header-compression/01-hpack-context/cmd/demo && cd go-solutions/44-capstone-http2-implementation/02-hpack-header-compression/01-hpack-context
go mod edit -go=1.26
go get golang.org/x/net/http2/hpack
```

### Why a context, and where the synchronization lives

The encoder writes into a private `bytes.Buffer` and is guarded by a mutex so two goroutines on the same connection cannot interleave their writes and corrupt a block. The decoder is a *persistent field*, not recreated per call, because the dynamic table must accumulate across every block exactly as the spec requires; recreating it would silently reset the table and desynchronize from the peer.

`Encode` enforces the never-indexed rule by header name: before writing each field it consults a `sensitiveHeaders` set and forces `Sensitive = true` for `authorization`, `cookie`, `set-cookie`, and `proxy-authorization`, overriding whatever the caller passed. This is the single chokepoint that makes a forgotten flag harmless. The library then emits the never-indexed representation, and the field round-trips with `Sensitive` set so the decoder side can see it was protected.

`Decode` installs an emit callback that runs once per decoded field. It accumulates each field's accounted `Size()` and, if a `maxHeaderBytes` bound was configured, aborts with `ErrHeaderListTooLarge` the moment the running total crosses it — the defence against a peer that sends a small compressed block which expands into a huge header list. After writing the block it always calls `Close()`, which validates the block's terminal state and resets per-block parsing without touching the dynamic table; skipping it would let an incomplete trailing integer from one block bleed into the next.

`SetMaxDynamicTableSize` only adjusts the encoder between blocks. The library emits the size-update pseudo-header at the start of the next `Encode`, so the decoder learns the new bound from the stream with no out-of-band coordination.

Create `context.go`:

```go
package hpcomp

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/net/http2/hpack"
)

var ErrHeaderListTooLarge = errors.New("hpcomp: header list exceeds maximum size")

var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"cookie":              true,
	"set-cookie":          true,
	"proxy-authorization": true,
}

type HPACKContext struct {
	mu             sync.Mutex
	enc            *hpack.Encoder
	encBuf         *bytes.Buffer
	dec            *hpack.Decoder
	maxHeaderBytes uint32
}

func NewHPACKContext(maxTableSize, maxHeaderBytes uint32) *HPACKContext {
	var encBuf bytes.Buffer
	enc := hpack.NewEncoder(&encBuf)
	enc.SetMaxDynamicTableSizeLimit(maxTableSize)
	enc.SetMaxDynamicTableSize(maxTableSize)

	dec := hpack.NewDecoder(maxTableSize, nil)
	dec.SetAllowedMaxDynamicTableSize(maxTableSize)

	return &HPACKContext{
		enc:            enc,
		encBuf:         &encBuf,
		dec:            dec,
		maxHeaderBytes: maxHeaderBytes,
	}
}

func (c *HPACKContext) Encode(fields []hpack.HeaderField) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.encBuf.Reset()
	for _, f := range fields {
		if sensitiveHeaders[f.Name] {
			f.Sensitive = true
		}
		if err := c.enc.WriteField(f); err != nil {
			return nil, fmt.Errorf("hpcomp: encode %q: %w", f.Name, err)
		}
	}
	out := make([]byte, c.encBuf.Len())
	copy(out, c.encBuf.Bytes())
	return out, nil
}

func (c *HPACKContext) Decode(block []byte) ([]hpack.HeaderField, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var (
		fields []hpack.HeaderField
		total  uint32
		decErr error
	)
	c.dec.SetEmitFunc(func(f hpack.HeaderField) {
		if decErr != nil {
			return
		}
		total += f.Size()
		if c.maxHeaderBytes > 0 && total > c.maxHeaderBytes {
			decErr = fmt.Errorf("%w: %d bytes", ErrHeaderListTooLarge, total)
			return
		}
		fields = append(fields, f)
	})
	if _, err := c.dec.Write(block); err != nil {
		return nil, fmt.Errorf("hpcomp: decode: %w", err)
	}
	if err := c.dec.Close(); err != nil {
		return nil, fmt.Errorf("hpcomp: decode close: %w", err)
	}
	if decErr != nil {
		return nil, decErr
	}
	return fields, nil
}

func (c *HPACKContext) SetMaxDynamicTableSize(size uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enc.SetMaxDynamicTableSize(size)
}

func (c *HPACKContext) MaxDynamicTableSize() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.MaxDynamicTableSize()
}

func IsSensitive(name string) bool {
	return sensitiveHeaders[name]
}
```


### The runnable demo

The demo replays three sequential request blocks on one connection. The first pays full price for `:authority`, `:path`, and the bearer token; the second and third reference the now-populated dynamic table and compress far better — the second-request gain HPACK is famous for. The `authorization` value is forced never-indexed, so it is re-sent in full each time and tagged `[never-indexed]` on the way out. The final lines show a table-size reduction taking effect on the encoder.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/hpcomp"
	"golang.org/x/net/http2/hpack"
)

func main() {
	ctx := hpcomp.NewHPACKContext(4096, 8192)

	requests := [][]hpack.HeaderField{
		{
			{Name: ":method", Value: "GET"},
			{Name: ":scheme", Value: "https"},
			{Name: ":authority", Value: "api.example.com"},
			{Name: ":path", Value: "/v1/users"},
			{Name: "accept", Value: "application/json"},
			{Name: "authorization", Value: "Bearer token-abc"},
		},
		{
			{Name: ":method", Value: "GET"},
			{Name: ":scheme", Value: "https"},
			{Name: ":authority", Value: "api.example.com"},
			{Name: ":path", Value: "/v1/users/42"},
			{Name: "accept", Value: "application/json"},
			{Name: "authorization", Value: "Bearer token-abc"},
		},
		{
			{Name: ":method", Value: "POST"},
			{Name: ":scheme", Value: "https"},
			{Name: ":authority", Value: "api.example.com"},
			{Name: ":path", Value: "/v1/users"},
			{Name: "content-type", Value: "application/json"},
			{Name: "authorization", Value: "Bearer token-abc"},
		},
	}

	for i, fields := range requests {
		encoded, err := ctx.Encode(fields)
		if err != nil {
			log.Fatalf("request %d: encode: %v", i+1, err)
		}
		decoded, err := ctx.Decode(encoded)
		if err != nil {
			log.Fatalf("request %d: decode: %v", i+1, err)
		}

		rawSize := 0
		for _, f := range fields {
			rawSize += len(f.Name) + len(f.Value) + 4
		}

		pct := 100 * float64(rawSize-len(encoded)) / float64(rawSize)
		fmt.Printf("request %d: %d raw bytes -> %d compressed bytes (%.0f%% reduction)\n",
			i+1, rawSize, len(encoded), pct)

		for _, f := range decoded {
			sens := ""
			if f.Sensitive {
				sens = " [never-indexed]"
			}
			fmt.Printf("  %s: %s%s\n", f.Name, f.Value, sens)
		}
	}

	fmt.Println("\ntable size reduced to 256 bytes:")
	ctx.SetMaxDynamicTableSize(256)
	fmt.Printf("  encoder table size: %d\n", ctx.MaxDynamicTableSize())
}
```


Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request 1: 136 raw bytes -> 55 compressed bytes (60% reduction)
  :method: GET
  :scheme: https
  :authority: api.example.com
  :path: /v1/users
  accept: application/json
  authorization: Bearer token-abc [never-indexed]
request 2: 139 raw bytes -> 30 compressed bytes (78% reduction)
  :method: GET
  :scheme: https
  :authority: api.example.com
  :path: /v1/users/42
  accept: application/json
  authorization: Bearer token-abc [never-indexed]
request 3: 143 raw bytes -> 32 compressed bytes (78% reduction)
  :method: POST
  :scheme: https
  :authority: api.example.com
  :path: /v1/users
  content-type: application/json
  authorization: Bearer token-abc [never-indexed]

table size reduced to 256 bytes:
  encoder table size: 256
```

The compression ratio jumps from 60% on the first request to 78% on the second because `:authority`, `accept`, and the path prefix are now indexed; the never-indexed `authorization` value is the main thing still paying full freight.

### Tests

The suite pins five properties. `TestRoundTripBasic` covers requests, a response, and an empty list. `TestSensitiveHeadersAreNeverIndexed` asserts the wrapper sets `Sensitive` even when the caller did not. `TestHeaderListSizeLimit` confirms an oversized decoded list returns `ErrHeaderListTooLarge`. `TestTableSizeReduction` drives a size update to zero and checks the next block still decodes. `TestRoundTripLargeTable` — the former "your turn" — runs 20 unique blocks through a 256-byte table so eviction churns continuously, pinning that eviction never desynchronizes the two tables.

Create `context_test.go`:

```go
package hpcomp

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/net/http2/hpack"
)

func TestRoundTripBasic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		fields []hpack.HeaderField
	}{
		{
			name: "standard GET request",
			fields: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":path", Value: "/"},
				{Name: ":scheme", Value: "https"},
				{Name: ":authority", Value: "example.com"},
			},
		},
		{
			name: "POST with content-type",
			fields: []hpack.HeaderField{
				{Name: ":method", Value: "POST"},
				{Name: ":path", Value: "/api/v1/items"},
				{Name: ":scheme", Value: "https"},
				{Name: "content-type", Value: "application/json"},
				{Name: "content-length", Value: "42"},
			},
		},
		{
			name: "response with status",
			fields: []hpack.HeaderField{
				{Name: ":status", Value: "200"},
				{Name: "content-type", Value: "text/html; charset=utf-8"},
				{Name: "cache-control", Value: "no-cache"},
			},
		},
		{
			name:   "empty header list",
			fields: []hpack.HeaderField{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := NewHPACKContext(4096, 0)
			encoded, err := ctx.Encode(tc.fields)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			decoded, err := ctx.Decode(encoded)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(decoded) != len(tc.fields) {
				t.Fatalf("decoded %d fields, want %d", len(decoded), len(tc.fields))
			}
			for i, want := range tc.fields {
				got := decoded[i]
				if got.Name != want.Name || got.Value != want.Value {
					t.Errorf("field[%d]: got %s=%s, want %s=%s",
						i, got.Name, got.Value, want.Name, want.Value)
				}
			}
		})
	}
}

func TestSensitiveHeadersAreNeverIndexed(t *testing.T) {
	t.Parallel()

	ctx := NewHPACKContext(4096, 0)
	fields := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: "authorization", Value: "Bearer secret-token"},
		{Name: "cookie", Value: "session=abc123"},
	}
	encoded, err := ctx.Encode(fields)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := ctx.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for _, f := range decoded {
		if IsSensitive(f.Name) && !f.Sensitive {
			t.Errorf("header %q: Sensitive = false, want true", f.Name)
		}
	}
}

func TestHeaderListSizeLimit(t *testing.T) {
	t.Parallel()

	ctx := NewHPACKContext(4096, 100)
	fields := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":path", Value: "/very/long/path/that/definitely/exceeds/the/limit"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "subdomain.example.com"},
		{Name: "accept", Value: "application/json, text/plain, */*"},
	}
	encoded, err := ctx.Encode(fields)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	_, err = ctx.Decode(encoded)
	if !errors.Is(err, ErrHeaderListTooLarge) {
		t.Fatalf("err = %v, want ErrHeaderListTooLarge", err)
	}
}

func TestTableSizeReduction(t *testing.T) {
	t.Parallel()

	ctx := NewHPACKContext(4096, 0)

	first := []hpack.HeaderField{
		{Name: "x-request-id", Value: "req-001"},
		{Name: "x-trace-id", Value: "trace-abc"},
	}
	encoded1, err := ctx.Encode(first)
	if err != nil {
		t.Fatalf("first Encode: %v", err)
	}
	if _, err := ctx.Decode(encoded1); err != nil {
		t.Fatalf("first Decode: %v", err)
	}

	ctx.SetMaxDynamicTableSize(0)
	if got := ctx.MaxDynamicTableSize(); got != 0 {
		t.Fatalf("MaxDynamicTableSize = %d, want 0", got)
	}

	second := []hpack.HeaderField{
		{Name: "x-request-id", Value: "req-002"},
		{Name: "x-trace-id", Value: "trace-def"},
	}
	encoded2, err := ctx.Encode(second)
	if err != nil {
		t.Fatalf("second Encode: %v", err)
	}
	decoded, err := ctx.Decode(encoded2)
	if err != nil {
		t.Fatalf("second Decode: %v", err)
	}
	if len(decoded) != len(second) {
		t.Fatalf("decoded %d fields, want %d", len(decoded), len(second))
	}
	for i, want := range second {
		if decoded[i].Name != want.Name || decoded[i].Value != want.Value {
			t.Errorf("field[%d]: got %s=%s, want %s=%s",
				i, decoded[i].Name, decoded[i].Value, want.Name, want.Value)
		}
	}
}

func TestRoundTripLargeTable(t *testing.T) {
	t.Parallel()

	// A 256-byte table holds only a handful of x-request-id entries at once, so
	// inserting 20 unique ones forces repeated eviction. The test pins that
	// eviction never desynchronizes encoder and decoder: every block decodes.
	ctx := NewHPACKContext(256, 0)
	for i := 0; i < 20; i++ {
		fields := []hpack.HeaderField{
			{Name: ":method", Value: "GET"},
			{Name: "x-request-id", Value: fmt.Sprintf("req-%03d", i)},
		}
		encoded, err := ctx.Encode(fields)
		if err != nil {
			t.Fatalf("block %d: Encode: %v", i, err)
		}
		decoded, err := ctx.Decode(encoded)
		if err != nil {
			t.Fatalf("block %d: Decode: %v", i, err)
		}
		if len(decoded) != 2 || decoded[1].Value != fmt.Sprintf("req-%03d", i) {
			t.Fatalf("block %d: decoded %+v", i, decoded)
		}
	}
}

func TestIsSensitive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		sensitive bool
	}{
		{"authorization", true},
		{"cookie", true},
		{"set-cookie", true},
		{"proxy-authorization", true},
		{":method", false},
		{":path", false},
		{"content-type", false},
		{"accept", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSensitive(tc.name); got != tc.sensitive {
				t.Errorf("IsSensitive(%q) = %v, want %v", tc.name, got, tc.sensitive)
			}
		})
	}
}

func ExampleNewHPACKContext() {
	ctx := NewHPACKContext(4096, 0)
	fields := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":path", Value: "/"},
	}
	encoded, _ := ctx.Encode(fields)
	decoded, _ := ctx.Decode(encoded)
	for _, f := range decoded {
		fmt.Printf("%s: %s\n", f.Name, f.Value)
	}
	// Output:
	// :method: GET
	// :scheme: https
	// :path: /
}
```


## Review

The context is correct when encode and decode stay synchronized across every block and the never-indexed enforcement is unconditional. Confirm the decoder is a persistent field so the dynamic table accumulates, confirm `Close` runs after every block so a trailing partial integer cannot leak forward, and confirm the size-limit check aborts the emit loop rather than letting a huge list materialize. The common mistakes are recreating the decoder per call (which silently resets the table), trusting callers to mark sensitive headers, and changing the encoder table size mid-block instead of between blocks. Running under `-race` is what proves the mutex actually covers every access to the shared buffer and decoder.

## Resources

- [RFC 7541 - HPACK](https://httpwg.org/specs/rfc7541.html) — the authoritative specification; sections 2-4 cover the formats, section 6 the wire representations, section 7 security.
- [golang.org/x/net/http2/hpack](https://pkg.go.dev/golang.org/x/net/http2/hpack) — the canonical Go encoder/decoder this module wraps, including `WriteField`, `SetEmitFunc`, and automatic size-update emission.
- [CRIME compression oracle](https://en.wikipedia.org/wiki/CRIME) — the attack that motivated never-indexed headers in RFC 7541 section 7.1.
- [HPACK: the silent killer feature of HTTP/2 (Cloudflare)](https://blog.cloudflare.com/hpack-the-silent-killer-feature-of-http-2/) — practical analysis of dynamic-table behaviour and the second-request gain.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-integer-coding.md](02-integer-coding.md)
