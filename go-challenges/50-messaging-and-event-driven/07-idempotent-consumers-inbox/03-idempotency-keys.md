# Exercise 3: Deriving Stable Idempotency Keys

The inbox can only dedup on a key that is *stable across redeliveries of the same
event*. This exercise builds the layer that decides what that key is: it prefers a
producer-supplied id when one exists, and otherwise falls back to a content hash so
that keyless or transport-reassigned duplicates still collapse to one key.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
idemkey/                   independent module: example.com/idemkey
  go.mod                   go 1.26
  idemkey.go               type Message, Source, DeriveKey; ErrEmptyPayload, ErrUnhashable
  cmd/
    demo/
      main.go              runnable demo: explicit id vs content hash
  idemkey_test.go          determinism, distinctness, preference tests, Example
```

- Files: `idemkey.go`, `cmd/demo/main.go`, `idemkey_test.go`.
- Implement: `DeriveKey(Message) (key string, source Source, err error)` that returns the explicit id verbatim when present (`SourceExplicit`), else canonicalizes the body with `encoding/json`, hashes with SHA-256, and hex-encodes it (`SourceContentHash`); sentinels `ErrEmptyPayload` and `ErrUnhashable`.
- Test: the same payload built in different map-insertion orders yields identical keys; different payloads yield different keys; an explicit id is used verbatim with `source==SourceExplicit`; an `Example` pinning a real 64-char hex key.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why the explicit id must win, and why the fallback is a hash

A producer-assigned id — a UUIDv7 or a business key carried in a header — is the
best possible idempotency key because it is *identity*, not *content*: it survives
transport reassignment (the same logical event keeps its id even if the broker
gives it a new offset), and it correctly treats two legitimately-identical events
as *distinct* when the producer says they are. Two separate `$5 top-up` requests
are different events with different ids; a content hash would wrongly fold them into
one. So when an id is present, `DeriveKey` returns it verbatim and reports
`SourceExplicit`.

The content-hash fallback is for the case where no id exists at all — a legacy
producer, a message whose header was stripped in transit. Hashing lets two copies
of the *same* payload (same event, new broker message number) collapse to one key,
which is better than treating every copy as new. But it carries the two hazards the
concepts named: it de-duplicates genuinely-identical events, and it demands a
*canonical* byte form. That second requirement is the subtle one.

### Canonicalization is the whole game

A hash is only stable if the bytes fed to it are stable. `fmt.Sprintf("%v", body)`
is not: a Go map iterates in randomized order, so the same map produces different
strings across runs, and the hash changes with it — dedup silently breaks. The fix
is `encoding/json`: its `Marshal` documents that map keys are *sorted*, so a
`map[string]any` serializes to the same bytes regardless of insertion order. That
sorted-key guarantee is precisely what makes the content hash deterministic. We
marshal the body, take `sha256.Sum256` over the result (which returns a `[32]byte`
array), slice it, and `hex.EncodeToString` to a 64-character string.

Two error paths guard the edges. A body of `nil` with no explicit id has nothing to
key on: `ErrEmptyPayload`. A body that `json.Marshal` cannot encode (a channel, a
function) yields `ErrUnhashable`, wrapping the marshal error with `%w` so the cause
is recoverable.

Create `idemkey.go`:

```go
package idemkey

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrEmptyPayload reports that a message had neither an explicit id nor a body
// to hash, so no key can be derived.
var ErrEmptyPayload = errors.New("idemkey: empty payload with no explicit id")

// ErrUnhashable reports that the body could not be canonicalized into bytes.
var ErrUnhashable = errors.New("idemkey: payload cannot be canonicalized")

// Source records how the key was chosen.
type Source int

const (
	// SourceExplicit means the producer-supplied id was used verbatim.
	SourceExplicit Source = iota
	// SourceContentHash means the key is a SHA-256 of the canonical body.
	SourceContentHash
)

// String renders a Source for logging.
func (s Source) String() string {
	switch s {
	case SourceExplicit:
		return "explicit"
	case SourceContentHash:
		return "content-hash"
	default:
		return "unknown"
	}
}

// Message is an inbound event. ID is the producer-supplied idempotency key and
// may be empty; Body is the payload used for the content-hash fallback.
type Message struct {
	ID   string
	Body any
}

// DeriveKey chooses the dedup key for msg. An explicit ID always wins and is
// returned verbatim with SourceExplicit. Otherwise the body is canonicalized
// with encoding/json (which sorts map keys, giving a deterministic byte form),
// hashed with SHA-256, and hex-encoded, reported as SourceContentHash.
func DeriveKey(msg Message) (key string, source Source, err error) {
	if msg.ID != "" {
		return msg.ID, SourceExplicit, nil
	}
	if msg.Body == nil {
		return "", SourceContentHash, ErrEmptyPayload
	}
	canonical, err := json.Marshal(msg.Body)
	if err != nil {
		return "", SourceContentHash, fmt.Errorf("%w: %w", ErrUnhashable, err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), SourceContentHash, nil
}
```

### The runnable demo

The demo derives a key for a message that carries an explicit id, then for one that
does not, so you can see the source switch from `explicit` to `content-hash`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/idemkey"
)

func main() {
	withID := idemkey.Message{ID: "evt-7f3a", Body: map[string]any{"amount": 100}}
	k1, s1, _ := idemkey.DeriveKey(withID)
	fmt.Printf("key=%s source=%s\n", k1, s1)

	noID := idemkey.Message{Body: map[string]any{"amount": 100, "currency": "USD"}}
	k2, s2, _ := idemkey.DeriveKey(noID)
	fmt.Printf("key=%s source=%s\n", k2, s2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
key=evt-7f3a source=explicit
key=9d1215b4ce08e5b8c77bccd7c2f673af82d153b1eabea22a1e3c524272b78db1 source=content-hash
```

### Tests

`TestDeterministicAcrossMapOrder` builds the same logical payload with keys inserted
in two different orders and asserts the derived keys are identical — the property
`json.Marshal`'s sorted keys guarantee. `TestDistinctPayloads` asserts differing
bodies yield differing keys. `TestExplicitIdWins` asserts an id is used verbatim and
that two messages with the *same* body but different ids stay distinct. The
`Example` pins the real hex output for a fixed payload.

Create `idemkey_test.go`:

```go
package idemkey

import (
	"errors"
	"fmt"
	"testing"
)

func TestDeterministicAcrossMapOrder(t *testing.T) {
	t.Parallel()
	a := map[string]any{}
	a["amount"] = 100
	a["currency"] = "USD"
	a["ref"] = "inv-1"

	b := map[string]any{}
	b["ref"] = "inv-1"
	b["currency"] = "USD"
	b["amount"] = 100

	ka, sa, err := DeriveKey(Message{Body: a})
	if err != nil {
		t.Fatalf("DeriveKey(a): %v", err)
	}
	kb, _, err := DeriveKey(Message{Body: b})
	if err != nil {
		t.Fatalf("DeriveKey(b): %v", err)
	}
	if ka != kb {
		t.Fatalf("keys differ across map order:\n a=%s\n b=%s", ka, kb)
	}
	if sa != SourceContentHash {
		t.Fatalf("source = %s; want content-hash", sa)
	}
	if len(ka) != 64 {
		t.Fatalf("hex key len = %d; want 64", len(ka))
	}
}

func TestDistinctPayloads(t *testing.T) {
	t.Parallel()
	k1, _, _ := DeriveKey(Message{Body: map[string]any{"amount": 100}})
	k2, _, _ := DeriveKey(Message{Body: map[string]any{"amount": 200}})
	if k1 == k2 {
		t.Fatalf("distinct payloads produced the same key: %s", k1)
	}
}

func TestExplicitIdWins(t *testing.T) {
	t.Parallel()
	body := map[string]any{"amount": 100}

	k1, s1, err := DeriveKey(Message{ID: "evt-1", Body: body})
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if k1 != "evt-1" || s1 != SourceExplicit {
		t.Fatalf("got key=%s source=%s; want evt-1 explicit", k1, s1)
	}

	// Same body, different producer ids: the explicit id keeps them distinct.
	k2, _, _ := DeriveKey(Message{ID: "evt-2", Body: body})
	if k1 == k2 {
		t.Fatal("same body with different ids collapsed to one key")
	}
}

func TestEmptyPayload(t *testing.T) {
	t.Parallel()
	_, _, err := DeriveKey(Message{})
	if !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("err = %v; want ErrEmptyPayload", err)
	}
}

func Example() {
	msg := Message{Body: map[string]any{"amount": 100, "currency": "USD"}}
	key, source, _ := DeriveKey(msg)
	fmt.Println(key)
	fmt.Println(source)
	// Output:
	// 9d1215b4ce08e5b8c77bccd7c2f673af82d153b1eabea22a1e3c524272b78db1
	// content-hash
}

// Your turn: add a test that a Body containing a value json.Marshal cannot encode
// (for example a chan int) returns an error that errors.Is(err, ErrUnhashable).
```

## Review

Key derivation is correct when the same logical event always maps to the same key
and distinct events never collide by accident. `TestDeterministicAcrossMapOrder` is
the load-bearing test: it only passes because `json.Marshal` sorts map keys, so two
maps with the same entries in different insertion orders serialize identically. Swap
the canonicalization for `fmt.Sprintf` and this test flakes — which is exactly the
production bug it guards against. `TestExplicitIdWins` proves the precedence rule:
an id is identity and always beats a content hash, so two genuinely-distinct events
that happen to share a body stay separate.

The mistakes to avoid: never content-hash an unstable serialization — a struct via
`%v` or a map via `fmt.Sprintf` breaks determinism silently. Prefer a producer id
whenever one exists; fall back to hashing only when it does not, and remember the
fallback wrongly de-duplicates two legitimately-identical events. And always hash a
canonical form: `json.Marshal`'s sorted keys give it for free, but a hand-rolled
serializer must sort its fields deliberately.

## Resources

- [encoding/json Marshal](https://pkg.go.dev/encoding/json#Marshal) — documents that map keys are sorted, the guarantee the content hash relies on.
- [crypto/sha256](https://pkg.go.dev/crypto/sha256) — `Sum256([]byte) [32]byte`, the fixed-size digest sliced for hex encoding.
- [encoding/hex EncodeToString](https://pkg.go.dev/encoding/hex#EncodeToString) — turning the digest into a stable string key.
- [Idempotent Consumer pattern - microservices.io](https://microservices.io/patterns/communication-style/idempotent-consumer.html) — where the derived key is used as the inbox primary key.

---

Back to [02-idempotent-consumer.md](02-idempotent-consumer.md) | Next: [../08-temporal-durable-execution/00-concepts.md](../08-temporal-durable-execution/00-concepts.md)
