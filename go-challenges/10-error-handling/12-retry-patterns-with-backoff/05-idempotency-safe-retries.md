# Exercise 5: Safe Retries Only — Idempotency Keys and the Double-Charge Bug

This is the module where a retry costs real money if you get it wrong. Retrying a
non-idempotent write after an ambiguous failure — you sent the charge, the response
was lost, you retry — double-charges the customer. This builds a payment client that
refuses to retry a write unless an idempotency key makes it safe, and a downstream
that deduplicates on that key so the retry is a no-op.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
idem/                      independent module: example.com/idem
  go.mod                   go 1.26
  idem.go                  ChargeServer (dedup by key); Client refusing unsafe retries
  cmd/
    demo/
      main.go              runnable demo: retries but charges exactly once
  idem_test.go             tests: one charge despite N attempts; same key each time; unsafe refused
```

Files: `idem.go`, `cmd/demo/main.go`, `idem_test.go`.
Implement: `NewKey()` via `crypto/rand`, a `ChargeServer` deduping by `Idempotency-Key`, and a `Client.Charge` that generates the key once, resends it on every attempt, and refuses to retry when `Idempotent` is false.
Test: N transient failures produce exactly ONE recorded charge (constant key); the same key is sent on every attempt; a non-idempotent client returns an error instead of retrying.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p go-solutions/10-error-handling/12-retry-patterns-with-backoff/05-idempotency-safe-retries/cmd/demo
cd go-solutions/10-error-handling/12-retry-patterns-with-backoff/05-idempotency-safe-retries
go mod edit -go=1.26
```

### The ambiguous-failure problem, and the key that solves it

Consider a POST that charges a card. The client sends it; the server processes the
charge; the response is lost on the way back (a dropped connection, a proxy timeout).
The client sees an error and cannot distinguish "the charge did not happen" from "the
charge happened but I did not hear back". If it retries, and the charge *did* happen,
the customer is charged twice. If it does *not* retry, and the charge did *not*
happen, the customer is not charged at all. Neither blind choice is acceptable.

The resolution is to make the operation idempotent so retrying is *always* safe,
regardless of what happened the first time. The client generates a random
**idempotency key** once, at the start of the logical operation, and sends it on
every attempt. The server keys its processing on it: the first request with a given
key is executed and its result stored; any later request with the same key returns
the *stored* result without re-executing. Now "retry after ambiguous failure" is
safe by construction — if the first attempt already charged, the retry returns the
same charge id and charges nothing more.

Three properties make or break this:

1. **The key is generated once and reused.** If the client regenerates the key per
   attempt, the server sees each attempt as a distinct operation and the
   deduplication does nothing — you are back to double-charging. The key must be
   stable across the whole retry sequence for one logical charge. Here it is created
   in `Charge` before the retry loop and captured by the closure.

2. **The key is unguessable and unique.** Use `crypto/rand`, not a counter or
   `math/rand`, so two concurrent operations never collide and a key cannot be
   forged. 16 bytes of `crypto/rand` hex-encoded is standard.

3. **Non-idempotent operations are refused, not retried.** If an operation genuinely
   cannot be made safe (no key, a legacy endpoint that does not dedup), the client
   must *not* retry it. The `Client.Idempotent` flag models this: when false, a
   failed write returns the error immediately rather than risking a duplicate. A
   retry you cannot make safe is a retry you must not perform.

The `ChargeServer` here is an in-memory stand-in for a real payments API: it records
every `(key, amount)` it actually charges and returns a stored id for a repeated key.
Its `attemptFailures` field lets a test make the first N calls fail *after* recording
nothing, simulating the transient-network case (the charge is refused, so a retry is
correct and safe).

Create `idem.go`:

```go
package idem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// ErrNonIdempotentRetry is returned when a write fails and the client is not
// allowed to retry it safely.
var ErrNonIdempotentRetry = errors.New("refusing to retry non-idempotent operation")

// ErrTransient marks a retryable network-level failure.
var ErrTransient = errors.New("transient failure")

// NewKey returns a random idempotency key (16 bytes, hex-encoded).
func NewKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error on supported platforms
	return hex.EncodeToString(b)
}

// ChargeServer is an in-memory payments backend that deduplicates by idempotency
// key: the first charge for a key is recorded; later charges with the same key
// return the stored result without charging again.
type ChargeServer struct {
	mu       sync.Mutex
	byKey    map[string]string // key -> chargeID
	charges  []charge          // every ACTUAL charge, for test assertions
	nextID   int
	failNext int // fail (transiently) the next N calls without recording
}

type charge struct {
	Key    string
	Amount int
}

func NewChargeServer() *ChargeServer {
	return &ChargeServer{byKey: make(map[string]string)}
}

// FailNext makes the next n Charge calls fail transiently before recording.
func (s *ChargeServer) FailNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = n
}

// Charge records a charge for key/amount and returns a charge id. A repeated key
// returns the stored id and records nothing new.
func (s *ChargeServer) Charge(key string, amount int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failNext > 0 {
		s.failNext--
		return "", fmt.Errorf("network drop: %w", ErrTransient)
	}
	if id, ok := s.byKey[key]; ok {
		return id, nil // deduplicated: no new charge
	}
	s.nextID++
	id := fmt.Sprintf("ch_%d", s.nextID)
	s.byKey[key] = id
	s.charges = append(s.charges, charge{Key: key, Amount: amount})
	return id, nil
}

// Charges returns a copy of every actual charge recorded, for assertions.
func (s *ChargeServer) Charges() []charge {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]charge, len(s.charges))
	copy(out, s.charges)
	return out
}

// Client charges through a ChargeServer with retries. When Idempotent is false it
// refuses to retry a failed write.
type Client struct {
	Server      *ChargeServer
	MaxAttempts int
	Idempotent  bool
}

// keysSent records every idempotency key the client sent, for the test that proves
// the key is constant across attempts.
type Result struct {
	ChargeID string
	KeysSent []string
	Attempts int
}

// Charge performs one logical charge, generating a single idempotency key reused on
// every retry. Returns ErrNonIdempotentRetry if a retry is needed but not allowed.
func (c *Client) Charge(ctx context.Context, amount int) (Result, error) {
	key := NewKey() // generated ONCE, before the loop
	var res Result
	var lastErr error
	for attempt := range c.MaxAttempts {
		res.Attempts = attempt + 1
		res.KeysSent = append(res.KeysSent, key)

		id, err := c.Server.Charge(key, amount)
		if err == nil {
			res.ChargeID = id
			return res, nil
		}
		lastErr = err
		if !errors.Is(err, ErrTransient) {
			return res, err
		}
		if !c.Idempotent {
			return res, fmt.Errorf("%w: %v", ErrNonIdempotentRetry, err)
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
	return res, lastErr
}
```

### The runnable demo

The demo makes the server fail the first two charge attempts transiently, then drives
an idempotent client. The retries succeed on the third attempt, and exactly one
charge is recorded.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/idem"
)

func main() {
	server := idem.NewChargeServer()
	server.FailNext(2) // first two attempts drop before recording

	client := &idem.Client{Server: server, MaxAttempts: 5, Idempotent: true}
	res, err := client.Charge(context.Background(), 4999)
	if err != nil {
		fmt.Println("charge failed:", err)
		return
	}

	fmt.Printf("charge id: %s after %d attempts\n", res.ChargeID, res.Attempts)
	fmt.Printf("charges recorded: %d\n", len(server.Charges()))
	fmt.Printf("same key every attempt: %v\n", allSame(res.KeysSent))
}

func allSame(keys []string) bool {
	for _, k := range keys {
		if k != keys[0] {
			return false
		}
	}
	return len(keys) > 0
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
charge id: ch_1 after 3 attempts
charges recorded: 1
same key every attempt: true
```

### Tests

The central test drives an idempotent client through transient failures and asserts
exactly ONE charge is recorded despite multiple attempts — the double-charge that a
naive retry would cause is prevented by the constant key. A second test asserts every
sent key is identical. The negative test configures `Idempotent: false` and asserts
the client returns `ErrNonIdempotentRetry` instead of retrying.

Create `idem_test.go`:

```go
package idem

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestExactlyOneChargeDespiteRetries(t *testing.T) {
	t.Parallel()
	server := NewChargeServer()
	server.FailNext(3) // three transient drops, then success

	client := &Client{Server: server, MaxAttempts: 5, Idempotent: true}
	res, err := client.Charge(context.Background(), 1000)
	if err != nil {
		t.Fatalf("Charge err = %v, want nil", err)
	}
	if got := len(server.Charges()); got != 1 {
		t.Fatalf("charges recorded = %d, want exactly 1 (no double charge)", got)
	}
	if res.Attempts != 4 {
		t.Fatalf("attempts = %d, want 4 (3 fail + 1 succeed)", res.Attempts)
	}
}

func TestSameKeyOnEveryAttempt(t *testing.T) {
	t.Parallel()
	server := NewChargeServer()
	server.FailNext(2)

	client := &Client{Server: server, MaxAttempts: 5, Idempotent: true}
	res, err := client.Charge(context.Background(), 2500)
	if err != nil {
		t.Fatalf("Charge err = %v, want nil", err)
	}
	if len(res.KeysSent) < 2 {
		t.Fatalf("only %d keys sent, want >= 2 to prove reuse", len(res.KeysSent))
	}
	for i, k := range res.KeysSent {
		if k != res.KeysSent[0] {
			t.Fatalf("attempt %d sent key %q, want constant %q", i, k, res.KeysSent[0])
		}
	}
}

func TestNonIdempotentRefusesRetry(t *testing.T) {
	t.Parallel()
	server := NewChargeServer()
	server.FailNext(1) // one transient failure

	client := &Client{Server: server, MaxAttempts: 5, Idempotent: false}
	_, err := client.Charge(context.Background(), 500)
	if !errors.Is(err, ErrNonIdempotentRetry) {
		t.Fatalf("err = %v, want ErrNonIdempotentRetry", err)
	}
	if got := len(server.Charges()); got != 0 {
		t.Fatalf("charges recorded = %d, want 0 (failed and refused to retry)", got)
	}
}

func TestKeysAreUniquePerOperation(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for range 1000 {
		k := NewKey()
		if seen[k] {
			t.Fatalf("duplicate key generated: %s", k)
		}
		seen[k] = true
	}
}

func ExampleClient_Charge() {
	server := NewChargeServer()
	server.FailNext(2)
	client := &Client{Server: server, MaxAttempts: 5, Idempotent: true}
	res, _ := client.Charge(context.Background(), 999)
	fmt.Println(len(server.Charges()), res.Attempts)
	// Output: 1 3
}
```

## Review

The client is correct when a retried charge results in exactly one recorded charge —
the deduplication depends entirely on the key being generated once and resent
unchanged, so if the "one charge" assertion ever fails, the key is being regenerated
per attempt. The negative path is equally important: a client that cannot make a
retry safe (`Idempotent: false`) must return `ErrNonIdempotentRetry` and leave the
side effect un-repeated, not silently retry. The mistakes this forecloses: retrying a
write with no idempotency key (double charge), and regenerating the key inside the
loop (defeats the server's dedup). Run `go test -race`; the `ChargeServer` guards its
maps with a mutex so concurrent charges are safe.

## Resources

- [`crypto/rand#Read`](https://pkg.go.dev/crypto/rand#Read) — unguessable key bytes.
- [`encoding/hex#EncodeToString`](https://pkg.go.dev/encoding/hex#EncodeToString) — hex key encoding.
- [Stripe: Idempotent Requests](https://docs.stripe.com/api/idempotent_requests) — how a real payments API deduplicates by `Idempotency-Key`.
- [Marc Brooker: Timeouts, Retries, and Idempotency](https://brooker.co.za/blog/2021/04/26/timeouts.html) — why retries require idempotency.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-deadline-budget-retry.md](04-deadline-budget-retry.md) | Next: [06-retryable-http-transport.md](06-retryable-http-transport.md)
