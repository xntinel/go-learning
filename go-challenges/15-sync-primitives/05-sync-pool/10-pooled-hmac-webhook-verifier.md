# Exercise 10: A Pooled HMAC-SHA256 Webhook Verifier With Constant-Time Compare

A webhook receiver verifies a signature on every delivery, and at high RPS the
expensive part is not hashing the body — it is `hmac.New`, which keys two
SHA-256 states per call. This module pools the keyed hashers so the keying
cost is paid once per hasher instead of once per request, while `Reset`
guarantees one delivery's payload never contaminates the next signature, and
`hmac.Equal` keeps the comparison constant-time.

This module is fully self-contained: its own `go mod init`, its own demo, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
webhookverify/                independent module: example.com/webhookverify
  go.mod                      go 1.26
  webhook/
    verifier.go               Verifier (per-instance pool of keyed hash.Hash); Sign; Middleware
    verifier_test.go          valid/tampered/truncated/non-hex table, Reset-wipe proof,
                              concurrency, Example, pooled-vs-New benchmark
  cmd/
    demo/
      main.go                 signs a delivery, replays it valid and tampered
```

Files: `webhook/verifier.go`, `webhook/verifier_test.go`, `cmd/demo/main.go`.
Implement: `NewVerifier(secret)` holding a `sync.Pool` of keyed `hmac.New(sha256.New, secret)` hashers; `Sign(body) string` (hex, what senders put in `X-Signature`); `Middleware(next)` that recomputes the MAC through a pooled hasher and rejects with 401 via `hmac.Equal` before the handler runs.
Test: valid signature reaches the inner handler; tampered body, truncated signature, non-hex signature, and missing header all yield 401 with the handler never invoked; the same body signs identically before and after a different body (Reset-wipe proof) and matches an independently computed HMAC; 300 concurrent distinct-payload requests under `-race`; a pooled-vs-`hmac.New`-per-request benchmark.
Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem -run=^$ ./webhook`

Set up the module:

```bash
mkdir -p webhookverify/webhook webhookverify/cmd/demo
cd webhookverify
go mod init example.com/webhookverify
```

### What the pool amortizes: keying, not hashing

HMAC-SHA256 derives two padded keys (ipad, opad) and absorbs each into its own
SHA-256 state — that is what `hmac.New(sha256.New, secret)` does, and it is
several allocations plus two compression-function runs before a single body
byte is hashed. Since every request to one endpoint uses the same secret, that
work is identical every time; the pool exists to stop repeating it. The
mechanism that makes this safe is the `hash.Hash` contract: "Reset resets the
Hash to its initial state" — and for an HMAC hasher the initial state is the
state *right after the key was absorbed* (crypto/hmac snapshots the keyed ipad
and opad states precisely so `Reset` can restore them cheaply). So
`Get -> Reset -> Write(body) -> Sum -> Put` reuses the expensive keyed state
for free while provably wiping the previous request's payload. The `Reset`
sits immediately after `Get`, the same reset-on-get discipline as every other
module here: a hasher that went back to the pool mid-write on some error path
must never leak those bytes into the next MAC.

One care point at construction: the pool's `New` closure captures the secret,
so `NewVerifier` clones the caller's slice first — a caller who zeroes or
reuses their secret buffer afterwards must not be able to corrupt future
hashers. And note the pool holds `hash.Hash` interface values whose dynamic
type is a pointer (`*hmac.hmac` internally), so `Put` never boxes — the SA6002
trap does not apply.

### The two 401 rules: fail closed, compare constant-time

The middleware reads the full body (it must — the MAC covers every byte),
recomputes the MAC, and only then hands off to the inner handler with the body
restored via `io.NopCloser(bytes.NewReader(body))`. Every rejection path is a
plain 401 with no detail: a signature endpoint that distinguishes "bad hex"
from "wrong MAC" hands an attacker an oracle for free. The comparison itself
is the classic one: `hmac.Equal`, never `==` on the hex strings and never
`bytes.Equal` — those return at the first differing byte, and a comparison
whose latency depends on how many leading bytes match lets an attacker forge
a signature byte by byte through timing. `hmac.Equal` wraps
`subtle.ConstantTimeCompare`, which examines every byte regardless. Decoding
the header with `hex.DecodeString` before comparing (rather than hex-encoding
the computed MAC) keeps the constant-time property where it matters and makes
malformed hex fail closed on its own branch.

Create `webhook/verifier.go`:

```go
// Package webhook verifies hex HMAC-SHA256 delivery signatures at high RPS,
// pooling keyed hashers so the per-request cost is a Reset instead of a
// full re-keying.
package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"net/http"
	"sync"
)

// Verifier authenticates webhook deliveries signed with a shared secret.
// Each Verifier owns its pool: hashers keyed for one secret must never be
// shared with a Verifier for another.
type Verifier struct {
	pool sync.Pool
}

// NewVerifier returns a Verifier for the given shared secret. The secret is
// cloned so a caller who later zeroes or reuses their slice cannot corrupt
// hashers the pool creates afterwards.
func NewVerifier(secret []byte) *Verifier {
	key := bytes.Clone(secret)
	v := &Verifier{}
	v.pool.New = func() any { return hmac.New(sha256.New, key) }
	return v
}

// mac computes the HMAC-SHA256 of body through a pooled keyed hasher. Reset
// restores the post-key state — the expensive keying is reused, the previous
// payload is wiped.
func (v *Verifier) mac(body []byte) []byte {
	h := v.pool.Get().(hash.Hash)
	h.Reset()
	h.Write(body)
	sum := h.Sum(nil)
	v.pool.Put(h)
	return sum
}

// Sign returns the hex HMAC-SHA256 of body: the value a sender puts in the
// X-Signature header. Receivers use it in tests and replay tooling.
func (v *Verifier) Sign(body []byte) string {
	return hex.EncodeToString(v.mac(body))
}

// Middleware rejects any request whose X-Signature header is not a valid hex
// HMAC-SHA256 of the body. Every failure is an undifferentiated 401 — a
// verifier that explains which check failed is an oracle. The comparison is
// hmac.Equal: constant-time, never == or bytes.Equal.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		want, err := hex.DecodeString(r.Header.Get("X-Signature"))
		if err != nil || len(want) == 0 {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if !hmac.Equal(v.mac(body), want) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

The demo signs a billing event, delivers it correctly (200, handler runs),
then replays the same signature over a tampered body (401, handler never
runs).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/webhookverify/webhook"
)

func main() {
	v := webhook.NewVerifier([]byte("wh_secret_2739"))

	handler := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "event accepted")
	}))

	body := `{"event":"invoice.paid","id":"evt_192"}`
	sig := v.Sign([]byte(body))
	fmt.Printf("signature=%s...\n", sig[:16])

	req := httptest.NewRequest(http.MethodPost, "/webhooks/billing", strings.NewReader(body))
	req.Header.Set("X-Signature", sig)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	fmt.Printf("signed delivery:   status=%d body=%q\n", rec.Code, rec.Body.String())

	forged := `{"event":"invoice.paid","id":"evt_FORGED"}`
	req = httptest.NewRequest(http.MethodPost, "/webhooks/billing", strings.NewReader(forged))
	req.Header.Set("X-Signature", sig)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	fmt.Printf("tampered delivery: status=%d\n", rec.Code)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
signature=bb9ee69e699a0784...
signed delivery:   status=200 body="event accepted"
tampered delivery: status=401
```

### Tests

The rejection table drives all the ways a signature check must fail closed,
with a flag proving the inner handler never ran. The Reset-wipe test is the
pool-specific one: it signs body A, contaminates the (likely recycled) hasher
with body B, signs A again, and demands identical output — then cross-checks
against an HMAC computed independently of the pool entirely. The concurrent
test hits one Verifier from 300 goroutines with distinct payloads; a hasher
handed to two requests at once, or a missed Reset, surfaces as a 401 under
`-race`.

Create `webhook/verifier_test.go`:

```go
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testSecret = "wh_secret_test"

func TestValidSignaturePasses(t *testing.T) {
	t.Parallel()

	v := NewVerifier([]byte(testSecret))
	var gotBody string
	handler := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))

	body := `{"event":"user.created"}`
	req := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader(body))
	req.Header.Set("X-Signature", v.Sign([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotBody != body {
		t.Fatalf("inner handler read %q, want %q (body not restored)", gotBody, body)
	}
}

func TestRejectedSignatures(t *testing.T) {
	t.Parallel()

	v := NewVerifier([]byte(testSecret))
	body := `{"event":"invoice.paid"}`
	valid := v.Sign([]byte(body))

	tests := []struct {
		name string
		body string
		sig  string
	}{
		{"tampered body", body + " ", valid},
		{"truncated signature", body, valid[:10]},
		{"non-hex signature", body, "zzzz-not-hex"},
		{"missing header", body, ""},
		{"signature for another secret", body, NewVerifier([]byte("other")).Sign([]byte(body))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			innerRan := false
			handler := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				innerRan = true
			}))
			req := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader(tt.body))
			if tt.sig != "" {
				req.Header.Set("X-Signature", tt.sig)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if innerRan {
				t.Fatal("inner handler ran on a rejected delivery")
			}
		})
	}
}

func TestResetWipesPreviousPayload(t *testing.T) {
	t.Parallel()

	v := NewVerifier([]byte(testSecret))
	a := []byte("payload A: the one we sign twice")
	b := []byte("payload B: the contaminant in between")

	first := v.Sign(a)
	_ = v.Sign(b) // dirty the pooled hasher with a different payload
	second := v.Sign(a)

	if first != second {
		t.Fatalf("same body signed differently before/after another payload:\n  %s\n  %s", first, second)
	}

	// Cross-check against an HMAC computed with no pool involved at all.
	ref := hmac.New(sha256.New, []byte(testSecret))
	ref.Write(a)
	if want := hex.EncodeToString(ref.Sum(nil)); first != want {
		t.Fatalf("pooled MAC = %s, independent MAC = %s", first, want)
	}
}

func TestConcurrentDistinctPayloads(t *testing.T) {
	t.Parallel()

	v := NewVerifier([]byte(testSecret))
	handler := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	const n = 300
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"event":"e-%d","payload":"distinct-%d"}`, i, i)
			req := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader(body))
			req.Header.Set("X-Signature", v.Sign([]byte(body)))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("request %d: status = %d, want 200 (hasher state bleed?)", i, rec.Code)
			}
		}()
	}
	wg.Wait()
}

func ExampleVerifier_Sign() {
	v := NewVerifier([]byte("example-secret"))
	fmt.Println(v.Sign([]byte("hello webhook")))
	// Output: fbd01f9de1199dca85af6fabf22f79a2d10bdfb9415f087198e23a530994683d
}

func BenchmarkPooledVerifier(b *testing.B) {
	v := NewVerifier([]byte(testSecret))
	body := []byte(`{"event":"invoice.paid","id":"evt_192"}`)
	b.ReportAllocs()
	for range b.N {
		if len(v.Sign(body)) == 0 {
			b.Fatal("empty signature")
		}
	}
}

func BenchmarkHMACNewPerRequest(b *testing.B) {
	secret := []byte(testSecret)
	body := []byte(`{"event":"invoice.paid","id":"evt_192"}`)
	b.ReportAllocs()
	for range b.N {
		h := hmac.New(sha256.New, secret) // full re-keying, every request
		h.Write(body)
		if len(hex.EncodeToString(h.Sum(nil))) == 0 {
			b.Fatal("empty signature")
		}
	}
}
```

Run the benchmarks:

```bash
go test -bench=. -benchmem -run=^$ ./webhook
```

The pooled path skips the per-request keying and its allocations; the
`hmac.New` path pays them every time — the delta shows up in both ns/op and
allocs/op.

## Review

The verifier is correct when a valid signature reaches the inner handler with
its body intact, every malformed or forged variant dies at 401 without the
handler running, and `TestResetWipesPreviousPayload` proves both halves of the
pooled-hasher claim: signing is deterministic across pool reuse, and it agrees
byte-for-byte with an HMAC computed with no pool at all. The two mistakes this
module guards against are quiet ones: skipping `Reset` after `Get` works
perfectly until the first time a hasher comes back dirty, and swapping
`hmac.Equal` for `==` on hex strings still passes every functional test while
silently reintroducing a timing oracle. Run `go test -count=1 -race ./...`.

## Resources

- [`crypto/hmac`](https://pkg.go.dev/crypto/hmac) — New, the keyed construction, and the constant-time Equal.
- [`hmac.Equal`](https://pkg.go.dev/crypto/hmac#Equal) — why MACs must never be compared with == or bytes.Equal.
- [`hash.Hash`](https://pkg.go.dev/hash#Hash) — the Reset-to-initial-state contract the pool relies on.
- [`encoding/hex.DecodeString`](https://pkg.go.dev/encoding/hex#DecodeString) — decoding the header so malformed hex fails closed.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-pooled-bufio-reader-line-protocol.md](09-pooled-bufio-reader-line-protocol.md) | Next: [../06-sync-cond/00-concepts.md](../06-sync-cond/00-concepts.md)
