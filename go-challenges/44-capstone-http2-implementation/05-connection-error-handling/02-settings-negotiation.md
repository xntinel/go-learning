# Exercise 2: SETTINGS Negotiation and the ACK Handshake

Every HTTP/2 connection opens with both peers exchanging a SETTINGS frame and acknowledging the other's. This module builds the `Settings` value type with its RFC 9113 §6.5.2 range validation and the `SettingsNegotiator` that drives the ACK handshake — storing the peer's parameters for lock-free reads and failing the connection with `SETTINGS_TIMEOUT` when an ACK never arrives.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
settings.go            ErrCode, ConnectionError, Settings, Validate, SettingsNegotiator
cmd/
  demo/
    main.go            reject a bad MaxFrameSize, negotiate good settings, ack
settings_test.go       defaults valid, range rejection, ack unblocks, timeout, idempotent ack
```

- Files: `settings.go`, `cmd/demo/main.go`, `settings_test.go`.
- Implement: `Settings` with `DefaultSettings`/`Validate`, and `SettingsNegotiator` with `ApplyRemote`, `AckReceived`, and `WaitForAck`.
- Test: `settings_test.go` checks the defaults validate, out-of-range values are rejected as connection errors, the ACK unblocks a waiter, a missing ACK times out, and a duplicate ACK does not panic.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p settings-negotiation/cmd/demo && cd settings-negotiation
go mod init example.com/settings-negotiation
go mod edit -go=1.26
```

### Validation is a connection-level gate

RFC 9113 §6.5.2 is explicit that an out-of-range SETTINGS value is a connection error, not a stream error: there is no per-stream context to scope it to, and a peer that advertises a nonsensical window size has misunderstood the protocol badly enough that continuing is unsafe. `Validate` therefore returns a `*ConnectionError` carrying the precise code the RFC assigns — `FLOW_CONTROL_ERROR` for an `InitialWindowSize` above 2^31-1, `PROTOCOL_ERROR` for a `MaxFrameSize` outside the legal [2^14, 2^24-1] band — wrapped around a sentinel so a caller can both branch on the disposition with `errors.As` and recognize the category with `errors.Is(err, ErrInvalidSetting)`. The defaults that `DefaultSettings` returns are the values that apply *before* any SETTINGS frame is exchanged, so they must themselves validate; a test pins that, because a typo in a default would make every fresh connection illegal.

### Why the negotiator splits reads from the ACK

The peer's settings are read on the hot path — every outgoing request consults `MaxConcurrentStreams` and `MaxFrameSize` — but written exactly once, when the peer's SETTINGS frame arrives. That read-heavy, write-rare shape is what `atomic.Pointer[Settings]` is for: readers load the current pointer with no lock and never block the rare `ApplyRemote` write, and `ApplyRemote` validates before it stores so a bad frame can never become the live settings. The ACK is a separate concern. `WaitForAck` blocks on a channel that `AckReceived` closes, and the close is guarded by `sync.Once` because the channel-close primitive panics if it runs twice and a buggy peer can send two ACK frames. If the context passed to `WaitForAck` expires first, it returns an error wrapping `ErrSettingsTimeout`, which the connection loop turns into a `SETTINGS_TIMEOUT` GOAWAY — the RFC's prescribed response to a peer that never acknowledges.

Create `settings.go`:

```go
// Package settings implements HTTP/2 SETTINGS negotiation (RFC 9113 §6.5):
// the six connection parameters, their range validation, and the ACK handshake
// both peers run immediately after the connection preface.
package settings

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrCode is the 32-bit error code carried in RST_STREAM and GOAWAY frames.
type ErrCode uint32

const (
	ErrCodeProtocolError    ErrCode = 0x1
	ErrCodeFlowControlError ErrCode = 0x3
	ErrCodeSettingsTimeout  ErrCode = 0x4
)

func (c ErrCode) String() string {
	switch c {
	case ErrCodeProtocolError:
		return "PROTOCOL_ERROR"
	case ErrCodeFlowControlError:
		return "FLOW_CONTROL_ERROR"
	case ErrCodeSettingsTimeout:
		return "SETTINGS_TIMEOUT"
	default:
		return fmt.Sprintf("UNKNOWN_ERROR_CODE_0x%x", uint32(c))
	}
}

// ConnectionError represents a violation that requires closing the connection.
type ConnectionError struct {
	Code  ErrCode
	Cause error
}

func (e *ConnectionError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("connection error %s: %v", e.Code, e.Cause)
	}
	return fmt.Sprintf("connection error %s", e.Code)
}

func (e *ConnectionError) Unwrap() error { return e.Cause }

const (
	settingMinFrameSize  = 1 << 14   // 16384: RFC 9113 §6.5.2
	settingMaxFrameSize  = 1<<24 - 1 // 16777215
	settingMaxWindowSize = 1<<31 - 1 // 2147483647: RFC 9113 §6.9.1
	settingDefaultWindow = 65535
)

// ErrInvalidSetting is the sentinel wrapped by validation errors.
var ErrInvalidSetting = errors.New("invalid SETTINGS value")

// Settings holds the six standard HTTP/2 connection parameters.
// The zero value is not valid; use DefaultSettings.
type Settings struct {
	HeaderTableSize      uint32
	EnablePush           bool
	MaxConcurrentStreams uint32 // 0 means "no limit" (RFC 9113 §6.5.2)
	InitialWindowSize    uint32
	MaxFrameSize         uint32
	MaxHeaderListSize    uint32 // 0 means "no limit"
}

// DefaultSettings returns the RFC 9113 initial values that apply before
// any SETTINGS frame has been exchanged.
func DefaultSettings() Settings {
	return Settings{
		HeaderTableSize:      4096,
		EnablePush:           true,
		MaxConcurrentStreams: 0, // unlimited
		InitialWindowSize:    settingDefaultWindow,
		MaxFrameSize:         settingMinFrameSize,
		MaxHeaderListSize:    0, // unlimited
	}
}

// Validate returns a *ConnectionError for any out-of-range value.
// RFC 9113 §6.5.2 mandates that the receiver treat out-of-range values as
// connection errors, not stream errors.
func (s Settings) Validate() error {
	if s.InitialWindowSize > settingMaxWindowSize {
		return &ConnectionError{
			Code: ErrCodeFlowControlError,
			Cause: fmt.Errorf("%w: InitialWindowSize %d exceeds 2^31-1",
				ErrInvalidSetting, s.InitialWindowSize),
		}
	}
	if s.MaxFrameSize < settingMinFrameSize || s.MaxFrameSize > settingMaxFrameSize {
		return &ConnectionError{
			Code: ErrCodeProtocolError,
			Cause: fmt.Errorf("%w: MaxFrameSize %d not in [2^14, 2^24-1]",
				ErrInvalidSetting, s.MaxFrameSize),
		}
	}
	return nil
}

// ErrSettingsTimeout is the sentinel returned when the peer does not
// acknowledge our SETTINGS frame within the configured deadline.
var ErrSettingsTimeout = errors.New("SETTINGS timeout")

// SettingsNegotiator manages the SETTINGS handshake at connection startup.
// Both peers send SETTINGS as their first frame after the connection preface;
// each peer must acknowledge the other's SETTINGS with a SETTINGS ACK.
//
// Remote settings are stored in an atomic.Pointer so reads from request
// handlers do not contend with the (rare) SETTINGS update write.
//
// Always construct via NewSettingsNegotiator; the zero value is not usable.
type SettingsNegotiator struct {
	local   Settings
	remote  atomic.Pointer[Settings]
	ackCh   chan struct{}
	ackOnce sync.Once
}

// NewSettingsNegotiator creates a negotiator advertising the given local
// settings. Remote settings are initialized to RFC defaults until the peer's
// SETTINGS frame is received and ApplyRemote is called.
func NewSettingsNegotiator(local Settings) *SettingsNegotiator {
	n := &SettingsNegotiator{
		local: local,
		ackCh: make(chan struct{}),
	}
	def := DefaultSettings()
	n.remote.Store(&def)
	return n
}

// Local returns the settings we advertised to the peer.
func (n *SettingsNegotiator) Local() Settings { return n.local }

// Remote returns the peer's current settings (defaults until ApplyRemote).
func (n *SettingsNegotiator) Remote() Settings { return *n.remote.Load() }

// ApplyRemote validates and stores the peer's SETTINGS.
// Returns a *ConnectionError if any value is out of range.
func (n *SettingsNegotiator) ApplyRemote(s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	n.remote.Store(&s)
	return nil
}

// AckReceived is called when the peer sends SETTINGS ACK, confirming
// that our local SETTINGS have been applied. Safe to call multiple times.
func (n *SettingsNegotiator) AckReceived() {
	n.ackOnce.Do(func() { close(n.ackCh) })
}

// WaitForAck blocks until a SETTINGS ACK is received or ctx is cancelled.
// Returns an error wrapping ErrSettingsTimeout if ctx expires first.
func (n *SettingsNegotiator) WaitForAck(ctx context.Context) error {
	select {
	case <-n.ackCh:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: SETTINGS ACK not received", ErrSettingsTimeout)
	}
}
```

### The runnable demo

The demo rejects a frame with an illegal `MaxFrameSize`, confirms the defaults validate, then applies a legal remote SETTINGS, acknowledges it, and waits for the ACK to unblock.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"example.com/settings-negotiation"
)

func main() {
	bad := settings.DefaultSettings()
	bad.MaxFrameSize = 100 // below the 16384 minimum
	if err := bad.Validate(); err != nil {
		fmt.Println("rejected:", err)
	}

	if err := settings.DefaultSettings().Validate(); err != nil {
		log.Fatal(err)
	}
	fmt.Println("default settings are valid")

	neg := settings.NewSettingsNegotiator(settings.DefaultSettings())
	remote := settings.DefaultSettings()
	remote.MaxConcurrentStreams = 256
	if err := neg.ApplyRemote(remote); err != nil {
		log.Fatal(err)
	}
	neg.AckReceived()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := neg.WaitForAck(ctx); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("negotiated: remote MaxConcurrentStreams=%d\n",
		neg.Remote().MaxConcurrentStreams)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rejected: connection error PROTOCOL_ERROR: invalid SETTINGS value: MaxFrameSize 100 not in [2^14, 2^24-1]
default settings are valid
negotiated: remote MaxConcurrentStreams=256
```

### Tests

The tests cover both halves. `TestDefaultSettingsAreValid` guards the defaults. The three `TestSettingsValidate*` functions reject a too-small frame size, a too-large frame size, and an oversized window, each asserting the right code and the wrapped sentinel. On the negotiator, `TestSettingsNegotiatorAckUnblocks` applies a remote and confirms the ACK releases a waiter; `TestSettingsNegotiatorTimeout*` confirms a short context yields `ErrSettingsTimeout`; `TestSettingsNegotiatorAckIsIdempotent` calls `AckReceived` three times to prove the `sync.Once` guard holds; and `TestSettingsNegotiatorRejectsInvalidRemote` confirms `ApplyRemote` refuses a bad frame.

Create `settings_test.go`:

```go
package settings

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDefaultSettingsAreValid(t *testing.T) {
	t.Parallel()
	if err := DefaultSettings().Validate(); err != nil {
		t.Fatalf("DefaultSettings().Validate() = %v", err)
	}
}

func TestSettingsValidateRejectsSmallMaxFrameSize(t *testing.T) {
	t.Parallel()
	s := DefaultSettings()
	s.MaxFrameSize = 100 // below minimum 16384
	err := s.Validate()
	if err == nil {
		t.Fatal("Validate() should reject MaxFrameSize=100")
	}
	if !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("err = %v, want wrapping ErrInvalidSetting", err)
	}
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Errorf("err = %v, want *ConnectionError", err)
	}
	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("code = %s, want PROTOCOL_ERROR", connErr.Code)
	}
}

func TestSettingsValidateRejectsLargeMaxFrameSize(t *testing.T) {
	t.Parallel()
	s := DefaultSettings()
	s.MaxFrameSize = 1 << 24 // 16777216, one above the max 16777215
	err := s.Validate()
	if err == nil {
		t.Fatal("Validate() should reject MaxFrameSize > 2^24-1")
	}
	if !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("err = %v, want wrapping ErrInvalidSetting", err)
	}
}

func TestSettingsValidateRejectsOversizedWindowSize(t *testing.T) {
	t.Parallel()
	s := DefaultSettings()
	s.InitialWindowSize = 1 << 31 // 2^31, exceeds max 2^31-1
	err := s.Validate()
	if err == nil {
		t.Fatal("Validate() should reject InitialWindowSize > 2^31-1")
	}
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Fatalf("err = %v, want *ConnectionError", err)
	}
	if connErr.Code != ErrCodeFlowControlError {
		t.Errorf("code = %s, want FLOW_CONTROL_ERROR", connErr.Code)
	}
}

func TestSettingsNegotiatorAckUnblocks(t *testing.T) {
	t.Parallel()
	neg := NewSettingsNegotiator(DefaultSettings())

	remote := DefaultSettings()
	remote.MaxConcurrentStreams = 100
	if err := neg.ApplyRemote(remote); err != nil {
		t.Fatalf("ApplyRemote: %v", err)
	}
	if got := neg.Remote().MaxConcurrentStreams; got != 100 {
		t.Errorf("Remote().MaxConcurrentStreams = %d, want 100", got)
	}

	neg.AckReceived()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := neg.WaitForAck(ctx); err != nil {
		t.Fatalf("WaitForAck after AckReceived: %v", err)
	}
}

func TestSettingsNegotiatorAckIsIdempotent(t *testing.T) {
	t.Parallel()
	neg := NewSettingsNegotiator(DefaultSettings())
	// Multiple AckReceived calls must not panic (closing a closed channel panics).
	neg.AckReceived()
	neg.AckReceived()
	neg.AckReceived()
}

func TestSettingsNegotiatorTimeoutWrapsErrSettingsTimeout(t *testing.T) {
	t.Parallel()
	neg := NewSettingsNegotiator(DefaultSettings())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := neg.WaitForAck(ctx)
	if !errors.Is(err, ErrSettingsTimeout) {
		t.Errorf("WaitForAck timeout: err = %v, want errors.Is(err, ErrSettingsTimeout)", err)
	}
}

func TestSettingsNegotiatorRejectsInvalidRemote(t *testing.T) {
	t.Parallel()
	neg := NewSettingsNegotiator(DefaultSettings())
	bad := DefaultSettings()
	bad.MaxFrameSize = 100
	err := neg.ApplyRemote(bad)
	if err == nil {
		t.Fatal("ApplyRemote with invalid settings should return error")
	}
	if !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("err = %v, want wrapping ErrInvalidSetting", err)
	}
}
```

## Review

The negotiator is correct when validation runs before any store and the ACK channel is closed exactly once. The first mistake is storing the remote settings and only then validating — by then a flow-control-breaking window size is already live for the next request; `ApplyRemote` must validate first and store nothing on failure. The second is closing the ACK channel directly: a duplicate ACK from a buggy peer then panics, which the `sync.Once` in `AckReceived` exists to prevent, and the idempotency test proves. Read the remote settings through the `atomic.Pointer` rather than a plain field so the hot path never takes a lock, and treat a `WaitForAck` timeout as the connection-fatal `SETTINGS_TIMEOUT` it is. Under `-race`, the concurrent loads in `Remote()` and the store in `ApplyRemote` are exactly the access pattern the detector validates.

## Resources

- [RFC 9113 §6.5 — SETTINGS](https://httpwg.org/specs/rfc9113.html#SETTINGS) — parameter definitions, default values, the ACK requirement, and the timeout rule.
- [RFC 9113 §6.5.2 — Defined Settings](https://httpwg.org/specs/rfc9113.html#SettingValues) — the per-parameter range constraints and the mandate to treat violations as connection errors.
- [sync/atomic — atomic.Pointer](https://pkg.go.dev/sync/atomic#Pointer) — the generic atomic pointer used for lock-free reads of the remote settings.
- [sync.Once](https://pkg.go.dev/sync#Once) — the one-shot guard that makes a duplicate ACK safe.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-error-classification.md](01-error-classification.md) | Next: [03-goaway-shutdown.md](03-goaway-shutdown.md)
