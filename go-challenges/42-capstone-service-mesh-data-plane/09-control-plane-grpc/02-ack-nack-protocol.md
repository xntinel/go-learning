# Exercise 2: ACK/NACK Protocol

The xDS stream is not fire-and-forget: every configuration the control plane pushes must be answered, and the answer is correlated to the exact response by a nonce. This exercise builds the two primitives that make that correlation work — a monotonic nonce generator and a per-resource-type ACK tracker that distinguishes an accepted version from a rejected one.

This module is fully self-contained: its own `go mod init`, all types defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
ack.go                ResourceType, NonceGen, AckTracker (Sent/Ack/Nack/LastAcked)
cmd/
  demo/
    main.go           push, ACK, NACK, and a rejected stale ACK
ack_test.go           nonce uniqueness, stale-nonce rejection, NACK leaves acked
```

- Files: `ack.go`, `cmd/demo/main.go`, `ack_test.go`.
- Implement: `NonceGen.Next()` and `AckTracker` with `Sent`, `Ack`, `Nack`, and `LastAcked`, all keyed by `ResourceType`.
- Test: nonces are unique and 16 hex chars, a matching `Ack` advances `LastAcked`, a stale nonce is rejected, and a `Nack` matches but does not advance `LastAcked`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p ack-protocol/cmd/demo && cd ack-protocol
go mod init example.com/ack-protocol
go mod edit -go=1.26
```

### Why a nonce, and why per-type state

A bidirectional stream multiplexes many requests and responses over one connection with no request/response pairing built in. The control plane can send three Endpoint updates in a row before the proxy answers any of them. So the proxy's answer has to name which response it is answering, and that name is the nonce. The control plane stamps each `DiscoveryResponse` with a fresh, monotonically increasing nonce; the proxy echoes that nonce back in its ACK or NACK; the control plane matches the echoed nonce against the last one it sent for that resource type. A nonce that does not match is stale — it acknowledges a response that has since been superseded — and is ignored. Modelling the nonce as a zero-padded 16-character hex counter makes it both unique within a stream and trivially sortable, which is convenient when reading a packet capture.

The tracker keeps two maps keyed by `ResourceType`: `pending` (the last nonce sent) and `acked` (the last nonce confirmed). The split between them is the whole point of ACK versus NACK. `Sent` records into `pending`. `Ack` checks the echoed nonce against `pending` and, on a match, copies it into `acked` — the proxy is now running that version. `Nack` checks the same `pending` match but deliberately does *not* touch `acked`: a rejection means the proxy could not apply the config and is still running whatever it last ACKed, so advancing `acked` would be a lie that hides configuration drift. `LastAcked` reads `acked`, returning empty for a type the proxy has never accepted. Keying everything by type is what lets a proxy be caught up on Listeners while three versions behind on Endpoints; a single global "last version" field could not represent that, and would let a NACK on one resource type silently roll back another.

Create `ack.go`:

```go
package ack

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// ResourceType is the full gRPC type URL used in xDS discovery requests and
// responses. These constants match the real Envoy v3 API type URLs.
type ResourceType string

const (
	TypeListener ResourceType = "type.googleapis.com/envoy.config.listener.v3.Listener"
	TypeRoute    ResourceType = "type.googleapis.com/envoy.config.route.v3.RouteConfiguration"
	TypeCluster  ResourceType = "type.googleapis.com/envoy.config.cluster.v3.Cluster"
	TypeEndpoint ResourceType = "type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment"
)

// NonceGen generates unique, monotonically increasing nonces for a single
// gRPC stream. Each DiscoveryResponse must carry a distinct nonce so the
// control plane can match a later ACK or NACK to a specific response.
type NonceGen struct{ n atomic.Uint64 }

// Next returns the next nonce as a zero-padded 16-character hex string.
func (g *NonceGen) Next() string { return fmt.Sprintf("%016x", g.n.Add(1)) }

// AckTracker records per-resource-type ACK state for a single connected
// client. The control plane calls Sent when it dispatches a response, Ack when
// the client accepts it, and Nack when the client rejects it.
type AckTracker struct {
	mu      sync.Mutex
	acked   map[ResourceType]string // last ACKed nonce per type
	pending map[ResourceType]string // last sent nonce per type
}

// NewAckTracker returns a tracker with no pending responses.
func NewAckTracker() *AckTracker {
	return &AckTracker{
		acked:   make(map[ResourceType]string),
		pending: make(map[ResourceType]string),
	}
}

// Sent records that a DiscoveryResponse with the given nonce was dispatched.
func (t *AckTracker) Sent(rt ResourceType, nonce string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending[rt] = nonce
}

// Ack records a successful application by the client. It returns true when the
// nonce matches the last sent response for rt; a false return means the nonce
// is stale and the ACK is ignored.
func (t *AckTracker) Ack(rt ResourceType, nonce string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending[rt] == nonce {
		t.acked[rt] = nonce
		return true
	}
	return false
}

// Nack records a rejected configuration. It returns true when the nonce matches
// the last sent response. After a NACK the acked map is NOT updated: the client
// is still on the previously ACKed version and the control plane must re-send.
func (t *AckTracker) Nack(rt ResourceType, nonce string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pending[rt] == nonce
}

// LastAcked returns the last nonce the client confirmed for rt; an empty string
// means the client has never ACKed this type.
func (t *AckTracker) LastAcked(rt ResourceType) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.acked[rt]
}
```

### The runnable demo

The demo walks one round of the protocol: push a Listener config and ACK it, push a Cluster config and NACK it (showing `LastAcked` stays empty), then reject a stale ACK that names the wrong nonce.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ack-protocol"
)

func main() {
	tracker := ack.NewAckTracker()
	nonces := &ack.NonceGen{}

	// 1. Server pushes a Listener config and records the nonce it sent.
	n1 := nonces.Next()
	tracker.Sent(ack.TypeListener, n1)
	fmt.Printf("pushed listeners with nonce=%s\n", n1)

	// 2. Client applies it and ACKs the same nonce.
	if tracker.Ack(ack.TypeListener, n1) {
		fmt.Printf("listeners ACKed: lastAcked=%s\n", tracker.LastAcked(ack.TypeListener))
	}

	// 3. Server pushes a Cluster config; the client rejects it (NACK).
	n2 := nonces.Next()
	tracker.Sent(ack.TypeCluster, n2)
	if tracker.Nack(ack.TypeCluster, n2) {
		fmt.Printf("clusters NACKed: lastAcked=%q (unchanged)\n", tracker.LastAcked(ack.TypeCluster))
	}

	// 4. A stale ACK (wrong nonce) is rejected.
	if !tracker.Ack(ack.TypeListener, "0000000000000000") {
		fmt.Println("stale ACK ignored")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pushed listeners with nonce=0000000000000001
listeners ACKed: lastAcked=0000000000000001
clusters NACKed: lastAcked="" (unchanged)
stale ACK ignored
```

### Tests

The tests pin every branch of the contract: a matching ACK advances `LastAcked`, a stale ACK does not, a matching NACK returns true but leaves `LastAcked` empty, state is tracked independently per type, and 200 nonces are all unique and correctly formatted.

Create `ack_test.go`:

```go
package ack

import "testing"

func TestAckMatchesPending(t *testing.T) {
	t.Parallel()
	tr := NewAckTracker()
	tr.Sent(TypeListener, "0000000000000001")
	if !tr.Ack(TypeListener, "0000000000000001") {
		t.Fatal("Ack returned false for matching nonce")
	}
	if got := tr.LastAcked(TypeListener); got != "0000000000000001" {
		t.Fatalf("LastAcked = %q, want the ACKed nonce", got)
	}
}

func TestAckStaleNonceReturnsFalse(t *testing.T) {
	t.Parallel()
	tr := NewAckTracker()
	tr.Sent(TypeListener, "0000000000000001")
	if tr.Ack(TypeListener, "0000000000000000") {
		t.Fatal("Ack returned true for stale nonce")
	}
	if got := tr.LastAcked(TypeListener); got != "" {
		t.Fatalf("LastAcked after stale Ack = %q, want empty", got)
	}
}

func TestNackDoesNotUpdateAcked(t *testing.T) {
	t.Parallel()
	tr := NewAckTracker()
	tr.Sent(TypeCluster, "0000000000000002")
	if !tr.Nack(TypeCluster, "0000000000000002") {
		t.Fatal("Nack returned false for matching nonce")
	}
	// After a NACK the client is still on the previously ACKed version.
	if got := tr.LastAcked(TypeCluster); got != "" {
		t.Fatalf("LastAcked after Nack = %q, want empty", got)
	}
}

func TestNackStaleNonceReturnsFalse(t *testing.T) {
	t.Parallel()
	tr := NewAckTracker()
	tr.Sent(TypeCluster, "0000000000000002")
	if tr.Nack(TypeCluster, "deadbeef") {
		t.Fatal("Nack returned true for stale nonce")
	}
}

func TestTracksPerType(t *testing.T) {
	t.Parallel()
	tr := NewAckTracker()
	tr.Sent(TypeListener, "nonce-L1")
	tr.Ack(TypeListener, "nonce-L1")
	tr.Sent(TypeCluster, "nonce-C1")
	tr.Ack(TypeCluster, "nonce-C1")
	if got := tr.LastAcked(TypeListener); got != "nonce-L1" {
		t.Fatalf("LastAcked(TypeListener) = %q, want nonce-L1", got)
	}
	if got := tr.LastAcked(TypeCluster); got != "nonce-C1" {
		t.Fatalf("LastAcked(TypeCluster) = %q, want nonce-C1", got)
	}
	if got := tr.LastAcked(TypeRoute); got != "" {
		t.Fatalf("LastAcked(TypeRoute) = %q, want empty", got)
	}
}

func TestNonceUniqueAndFormatted(t *testing.T) {
	t.Parallel()
	g := &NonceGen{}
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		n := g.Next()
		if len(n) != 16 {
			t.Fatalf("nonce %q has length %d, want 16", n, len(n))
		}
		if seen[n] {
			t.Fatalf("duplicate nonce %q at call %d", n, i)
		}
		seen[n] = true
	}
}
```

## Review

The tracker is correct when ACK and NACK share the same nonce-matching guard but differ in exactly one effect: ACK advances `acked`, NACK does not. The single most consequential mistake is letting NACK update `acked` — it makes the control plane believe a rejected config is live, and the proxy silently runs stale routing while the server stops re-sending. `TestNackDoesNotUpdateAcked` is the guard against that. The stale-nonce tests catch the other classic error: skipping the `pending` comparison and treating any ACK as valid, which lets a delayed acknowledgment of an obsolete response advance the version past a config the proxy never actually applied. Keying both maps by `ResourceType` is what `TestTracksPerType` verifies — an untouched type reads as empty, and an ACK on one type never disturbs another. The nonce generator's only jobs are uniqueness and format; `TestNonceUniqueAndFormatted` confirms 200 calls collide on neither.

## Resources

- [xDS REST and gRPC protocol: ACK/NACK and nonce semantics](https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol) — the normative description of how the nonce correlates a response with its acknowledgment.
- [`fmt` verbs: `%016x`](https://pkg.go.dev/fmt#hdr-Printing) — zero-padded fixed-width hex formatting used for the nonce.
- [`sync/atomic`: `Uint64.Add`](https://pkg.go.dev/sync/atomic#Uint64.Add) — the lock-free monotonic counter behind `NonceGen`.

---

Back to [01-config-snapshot-store.md](01-config-snapshot-store.md) | Next: [03-reconnect-state-machine.md](03-reconnect-state-machine.md)
