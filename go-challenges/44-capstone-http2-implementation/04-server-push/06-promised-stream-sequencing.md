# Exercise 6: Promised-Stream-ID Sequencing

The stream-ID rules are the easiest part of server push to get subtly wrong and the most expensive to get wrong, because a violation is a connection error that destroys every stream. This module is a single validator that enforces all of it in one place: the associated stream must be client-initiated, the promised stream must be server-initiated, and promised IDs must strictly increase.

This module is fully self-contained: its own `go mod init`, all code inline, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
promised-stream-sequencing/
  go.mod
  validator.go         Validator, Validate, LastPromised, three error sentinels
  validator_test.go    increasing sequence, gaps allowed, non-increasing, odd/zero, even assoc
  cmd/demo/main.go     a valid sequence then four rejected promises
```

- Files: `validator.go`, `validator_test.go`, `cmd/demo/main.go`.
- Implement: a `Validator` with `Validate(associatedID, promisedID uint32) error` and `LastPromised() uint32`, plus the sentinels `ErrAssociatedNotClient`, `ErrPromisedNotServer`, `ErrPromisedNotIncreasing`.
- Test: an even, increasing sequence is accepted; gaps are allowed; a non-increasing ID is rejected and does not advance the mark; odd and zero promised IDs are rejected; an even or zero associated ID is rejected; concurrent validation stays monotonic under `-race`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p promised-stream-sequencing/cmd/demo && cd promised-stream-sequencing
go mod init example.com/promised-stream-sequencing
go mod edit -go=1.26
```

### Three rules, one ordered check

`Validate` enforces three rules from RFC 9113 §5.1.1 and §6.6, and the order it checks them in is part of the contract. First, the associated stream — the client stream the PUSH_PROMISE is delivered on — must be client-initiated, which means odd and non-zero; a PUSH_PROMISE on stream 0 or on an even (server) stream is illegal because a server cannot promise against a stream a client never opened. Second, the promised stream must be server-initiated: even and non-zero, because pushed responses live exclusively in the even-ID space. Third, the promised ID must strictly exceed every promised ID seen before on this connection, because §5.1.1 requires a newly opened stream ID to be numerically greater than all streams the initiating endpoint has already opened. The parity and non-zero checks come before the monotonicity check so that an ID failing on multiple counts is reported by its most fundamental fault first.

Two design decisions make the validator trustworthy. It returns distinct sentinel errors wrapped with the offending IDs, so a caller can `errors.Is` against `ErrAssociatedNotClient`, `ErrPromisedNotServer`, or `ErrPromisedNotIncreasing` to decide how to react while still logging the concrete numbers — all three map to a PROTOCOL_ERROR on the wire, but the sentinel tells you which invariant broke. And it advances its high-water mark only on success: a rejected promise leaves `lastPromised` untouched, so a bad ID cannot corrupt the sequence state and a later valid ID is still judged against the last *accepted* one. The mutex makes `Validate` and `LastPromised` safe to call concurrently, which matters when several handler goroutines on one connection each reserve a push, and it guarantees the monotonicity check and the mark update are one atomic step so two concurrent validations can never both accept the same ID.

Create `validator.go`:

```go
package sequencing

import (
	"errors"
	"fmt"
	"sync"
)

// These sentinels classify the four ways a PUSH_PROMISE can violate the stream
// identifier rules of RFC 9113 §5.1.1 and §6.6. Callers match them with
// errors.Is; each maps to a PROTOCOL_ERROR connection error on the wire.
var (
	// ErrAssociatedNotClient marks an associated stream ID that is not a valid
	// client-initiated stream (must be odd and non-zero).
	ErrAssociatedNotClient = errors.New("sequencing: associated stream is not client-initiated (odd, non-zero)")

	// ErrPromisedNotServer marks a promised stream ID that is not a valid
	// server-initiated stream (must be even and non-zero).
	ErrPromisedNotServer = errors.New("sequencing: promised stream is not server-initiated (even, non-zero)")

	// ErrPromisedNotIncreasing marks a promised stream ID that is not strictly
	// greater than every previously promised ID on this connection.
	ErrPromisedNotIncreasing = errors.New("sequencing: promised stream ID does not strictly increase")
)

// Validator enforces the promised-stream-ID rules for one connection. A server
// allocates even, strictly increasing IDs for pushed streams; this type is the
// receiver-side check a correct client performs, and the self-check a server
// runs before sending PUSH_PROMISE. The zero value is ready to use.
type Validator struct {
	mu           sync.Mutex
	lastPromised uint32 // highest promised ID accepted so far; 0 means none yet
}

// Validate checks one PUSH_PROMISE: associatedID is the client stream the
// promise is delivered on, promisedID is the new server stream that will carry
// the pushed response. On success it records promisedID as the new high-water
// mark. On failure it returns one of the package sentinels wrapped with the
// offending IDs, and does not advance the high-water mark.
//
// The rules (RFC 9113 §5.1.1, §6.6):
//   - associatedID is client-initiated: odd and non-zero.
//   - promisedID is server-initiated: even and non-zero.
//   - promisedID strictly exceeds every previously promised ID, because a new
//     stream ID must be numerically greater than all streams the initiating
//     endpoint has opened.
func (v *Validator) Validate(associatedID, promisedID uint32) error {
	if associatedID == 0 || associatedID%2 == 0 {
		return fmt.Errorf("associated=%d: %w", associatedID, ErrAssociatedNotClient)
	}
	if promisedID == 0 || promisedID%2 != 0 {
		return fmt.Errorf("promised=%d: %w", promisedID, ErrPromisedNotServer)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if promisedID <= v.lastPromised {
		return fmt.Errorf("promised=%d not greater than last=%d: %w",
			promisedID, v.lastPromised, ErrPromisedNotIncreasing)
	}
	v.lastPromised = promisedID
	return nil
}

// LastPromised returns the highest promised stream ID accepted so far, or 0 if
// none has been validated yet.
func (v *Validator) LastPromised() uint32 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.lastPromised
}
```

### The runnable demo

The demo validates a clean sequence of even, increasing promised IDs on client stream 1, then runs four rejected promises — reusing an ID, an odd promised ID, an even associated ID, and a zero promised ID — classifying each by its sentinel, and finally shows that none of the rejections advanced the high-water mark.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	seq "example.com/promised-stream-sequencing"
)

func main() {
	var v seq.Validator

	// A well-formed sequence of pushes on client stream 1: even, increasing.
	fmt.Println("valid sequence on associated stream 1:")
	for _, pid := range []uint32{2, 4, 6} {
		if err := v.Validate(1, pid); err != nil {
			fmt.Printf("  promised %d -> %v\n", pid, err)
		} else {
			fmt.Printf("  promised %d -> ok (last=%d)\n", pid, v.LastPromised())
		}
	}

	fmt.Println("rejected promises:")

	// Re-using an ID: not strictly increasing.
	err := v.Validate(1, 4)
	fmt.Printf("  reuse 4:        %s\n", classify(err))

	// Odd promised ID: not server-initiated.
	err = v.Validate(1, 7)
	fmt.Printf("  odd 7:          %s\n", classify(err))

	// Even associated ID: not client-initiated.
	err = v.Validate(2, 8)
	fmt.Printf("  assoc 2:        %s\n", classify(err))

	// Zero promised ID.
	err = v.Validate(1, 0)
	fmt.Printf("  promised 0:     %s\n", classify(err))

	fmt.Printf("high-water mark unchanged: last=%d\n", v.LastPromised())
}

func classify(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, seq.ErrAssociatedNotClient):
		return "PROTOCOL_ERROR (associated not client-initiated)"
	case errors.Is(err, seq.ErrPromisedNotServer):
		return "PROTOCOL_ERROR (promised not server-initiated)"
	case errors.Is(err, seq.ErrPromisedNotIncreasing):
		return "PROTOCOL_ERROR (promised not increasing)"
	default:
		return err.Error()
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid sequence on associated stream 1:
  promised 2 -> ok (last=2)
  promised 4 -> ok (last=4)
  promised 6 -> ok (last=6)
rejected promises:
  reuse 4:        PROTOCOL_ERROR (promised not increasing)
  odd 7:          PROTOCOL_ERROR (promised not server-initiated)
  assoc 2:        PROTOCOL_ERROR (associated not client-initiated)
  promised 0:     PROTOCOL_ERROR (promised not server-initiated)
high-water mark unchanged: last=6
```

Each rejection is classified by the rule it broke. The `assoc 2` line is the call `Validate(2, 8)`: the even associated ID fails the associated-stream check, which runs before the promised-stream and monotonicity checks, so it reports `ErrAssociatedNotClient` even though the promised ID 8 is itself valid. The mark stays at 6 because not one rejected promise advanced it.

### Tests

The tests pin the accepted case (even and increasing, with gaps allowed since IDs need only increase, not be contiguous) and every rejection: a non-increasing ID, an odd promised ID, a zero promised ID, an even associated ID, and a zero associated ID. `TestRejectsNonIncreasing` also asserts the mark is unchanged after rejections. `TestConcurrentValidateIsMonotonic` validates two hundred IDs from two hundred goroutines and asserts the final mark is the maximum, proving the validator stays consistent under `-race`.

Create `validator_test.go`:

```go
package sequencing

import (
	"errors"
	"sync"
	"testing"
)

func TestIncreasingEvenSequence(t *testing.T) {
	t.Parallel()
	var v Validator
	for _, pid := range []uint32{2, 4, 6, 100} {
		if err := v.Validate(1, pid); err != nil {
			t.Fatalf("Validate(1, %d) = %v, want nil", pid, err)
		}
	}
	if v.LastPromised() != 100 {
		t.Fatalf("LastPromised = %d, want 100", v.LastPromised())
	}
}

func TestGapsAreAllowed(t *testing.T) {
	t.Parallel()
	var v Validator
	// IDs need only strictly increase; they need not be contiguous.
	if err := v.Validate(1, 2); err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(3, 8); err != nil {
		t.Fatalf("a gap from 2 to 8 must be allowed: %v", err)
	}
}

func TestRejectsNonIncreasing(t *testing.T) {
	t.Parallel()
	var v Validator
	if err := v.Validate(1, 6); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []uint32{6, 4, 2} {
		err := v.Validate(1, pid)
		if !errors.Is(err, ErrPromisedNotIncreasing) {
			t.Fatalf("Validate(1, %d) = %v, want ErrPromisedNotIncreasing", pid, err)
		}
	}
	if v.LastPromised() != 6 {
		t.Fatalf("a rejected promise must not advance the mark: last=%d", v.LastPromised())
	}
}

func TestRejectsOddPromised(t *testing.T) {
	t.Parallel()
	var v Validator
	if err := v.Validate(1, 3); !errors.Is(err, ErrPromisedNotServer) {
		t.Fatalf("err = %v, want ErrPromisedNotServer", err)
	}
}

func TestRejectsZeroPromised(t *testing.T) {
	t.Parallel()
	var v Validator
	if err := v.Validate(1, 0); !errors.Is(err, ErrPromisedNotServer) {
		t.Fatalf("err = %v, want ErrPromisedNotServer", err)
	}
}

func TestRejectsEvenAssociated(t *testing.T) {
	t.Parallel()
	var v Validator
	if err := v.Validate(2, 4); !errors.Is(err, ErrAssociatedNotClient) {
		t.Fatalf("err = %v, want ErrAssociatedNotClient", err)
	}
}

func TestRejectsZeroAssociated(t *testing.T) {
	t.Parallel()
	var v Validator
	if err := v.Validate(0, 2); !errors.Is(err, ErrAssociatedNotClient) {
		t.Fatalf("err = %v, want ErrAssociatedNotClient", err)
	}
}

func TestConcurrentValidateIsMonotonic(t *testing.T) {
	t.Parallel()
	var v Validator
	const n = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < n; i++ {
		pid := uint32(2 * (i + 1))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := v.Validate(1, pid); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Whatever the interleaving, the final mark is the maximum even ID and the
	// validator never panicked or corrupted its state under -race.
	if got := v.LastPromised(); got != uint32(2*n) {
		t.Fatalf("LastPromised = %d, want %d", got, 2*n)
	}
	if accepted == 0 {
		t.Fatal("expected at least some concurrent validations to be accepted")
	}
}
```

## Review

The validator is correct when a rejected promise never advances the high-water mark — `TestRejectsNonIncreasing` is the guard, and an implementation that updated `lastPromised` before the monotonicity check would fail it. Confirm the three checks run in order (associated parity, promised parity, then monotonicity) and that each returns a distinct sentinel so callers can branch with `errors.Is`. Run the suite under `-race`: `TestConcurrentValidateIsMonotonic` is what proves the monotonicity check and the mark update are one atomic step, so two concurrent calls can never both accept the same ID. The bugs this module rules out are the most common server-push framing faults — reusing or rewinding a stream ID, putting a pushed response on an odd (client) ID, or promising against an even (server) associated stream — each of which is a PROTOCOL_ERROR that would otherwise tear down the connection.

## Resources

- [RFC 9113 §5.1.1 — Stream Identifiers](https://httpwg.org/specs/rfc9113.html#StreamIdentifiers) — even vs odd initiators, stream 0, and the strictly-increasing rule.
- [RFC 9113 §6.6 — PUSH_PROMISE](https://httpwg.org/specs/rfc9113.html#PUSH_PROMISE) — the associated-stream and promised-stream-ID requirements.
- [`errors.Is` and wrapping](https://pkg.go.dev/errors#Is) — the sentinel-matching the validator's typed errors are built for.

---

Back to [05-rst-stream-cancel.md](05-rst-stream-cancel.md) | Next: [Connection and Error Handling](../05-connection-error-handling/00-concepts.md)
