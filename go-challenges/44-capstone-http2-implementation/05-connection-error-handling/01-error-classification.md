# Exercise 1: Error Classification and the Error-Code Taxonomy

The first decision an HTTP/2 endpoint makes about any protocol violation is whether it killed one stream or the whole connection. This module builds the full RFC 9113 §7 error-code taxonomy and the single pure function, `ClassifyError`, that maps a `(frameType, streamID, errorCode, cause)` tuple to either a stream-scoped `*StreamError` (answered with `RST_STREAM`) or a connection-scoped `*ConnectionError` (answered with `GOAWAY`).

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
errclass.go            ErrCode + names, StreamError, ConnectionError, FrameType, ClassifyError
cmd/
  demo/
    main.go            classify a compression error and a stream cancel
errclass_test.go       code names, the four classification rules, error unwrapping
```

- Files: `errclass.go`, `cmd/demo/main.go`, `errclass_test.go`.
- Implement: the `ErrCode` type with its RFC names, the `StreamError`/`ConnectionError` wrappers, the `FrameType` constants, and `ClassifyError`.
- Test: `errclass_test.go` checks every code name, the four rules that force a connection error, the stream-error default, and that both wrappers unwrap their cause.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p error-classification/cmd/demo && cd error-classification
go mod init example.com/error-classification
go mod edit -go=1.26
```

### Why a single pure classifier

Scattering "is this fatal?" checks across a frame reader guarantees they will drift: one code path sends `RST_STREAM` for a compression error while another sends `GOAWAY`, and the connection state diverges from what the peer believes. Concentrating the entire policy in one pure function makes it auditable and testable in isolation — it takes four values and returns an error, touches no shared state, and never blocks. The policy is the small predicate from RFC 9113 §5.4: a frame is fatal to the connection if its code is `COMPRESSION_ERROR` (HPACK state is connection-scoped, so once it is corrupted every future HEADERS frame is undecodable), or it arrived on a SETTINGS/PING/GOAWAY frame (those are inherently connection-level), or it arrived on stream 0 (the connection control channel, which has no per-stream state). Anything else on a non-zero stream is a stream error, isolated to that one stream.

The two error types are thin wrappers that carry the disposition in their Go type, so a caller can branch with `errors.As(err, &connErr)` rather than re-deriving the classification. Both implement `Unwrap` so an underlying cause — a decoder's own error, say — survives `errors.Is`/`errors.As` all the way up the stack. The `ErrCode.String` method gives every code its RFC name for logs, with a hex fallback for the unknown codes a future RFC might add, so an unrecognized code is still printable rather than a bare integer.

Create `errclass.go`:

```go
// Package errclass implements HTTP/2 error classification: the RFC 9113 §7
// error-code taxonomy and the rule (RFC 9113 §5.4) that decides whether a
// protocol violation is scoped to one stream (RST_STREAM) or fatal to the
// whole connection (GOAWAY).
package errclass

import "fmt"

// ErrCode is the 32-bit error code carried in RST_STREAM and GOAWAY frames.
type ErrCode uint32

const (
	ErrCodeNoError            ErrCode = 0x0
	ErrCodeProtocolError      ErrCode = 0x1
	ErrCodeInternalError      ErrCode = 0x2
	ErrCodeFlowControlError   ErrCode = 0x3
	ErrCodeSettingsTimeout    ErrCode = 0x4
	ErrCodeStreamClosed       ErrCode = 0x5
	ErrCodeFrameSizeError     ErrCode = 0x6
	ErrCodeRefusedStream      ErrCode = 0x7
	ErrCodeCancel             ErrCode = 0x8
	ErrCodeCompressionError   ErrCode = 0x9
	ErrCodeConnectError       ErrCode = 0xa
	ErrCodeEnhanceYourCalm    ErrCode = 0xb
	ErrCodeInadequateSecurity ErrCode = 0xc
	ErrCodeHTTP11Required     ErrCode = 0xd
)

var errCodeNames = map[ErrCode]string{
	ErrCodeNoError:            "NO_ERROR",
	ErrCodeProtocolError:      "PROTOCOL_ERROR",
	ErrCodeInternalError:      "INTERNAL_ERROR",
	ErrCodeFlowControlError:   "FLOW_CONTROL_ERROR",
	ErrCodeSettingsTimeout:    "SETTINGS_TIMEOUT",
	ErrCodeStreamClosed:       "STREAM_CLOSED",
	ErrCodeFrameSizeError:     "FRAME_SIZE_ERROR",
	ErrCodeRefusedStream:      "REFUSED_STREAM",
	ErrCodeCancel:             "CANCEL",
	ErrCodeCompressionError:   "COMPRESSION_ERROR",
	ErrCodeConnectError:       "CONNECT_ERROR",
	ErrCodeEnhanceYourCalm:    "ENHANCE_YOUR_CALM",
	ErrCodeInadequateSecurity: "INADEQUATE_SECURITY",
	ErrCodeHTTP11Required:     "HTTP_1_1_REQUIRED",
}

// String returns the RFC 9113 name for the error code or a hex fallback.
func (c ErrCode) String() string {
	if name, ok := errCodeNames[c]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN_ERROR_CODE_0x%x", uint32(c))
}

// StreamError represents a violation that affects only one stream.
// The correct response is RST_STREAM; other streams are unaffected.
type StreamError struct {
	StreamID uint32
	Code     ErrCode
	Cause    error
}

func (e *StreamError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("stream %d: %s: %v", e.StreamID, e.Code, e.Cause)
	}
	return fmt.Sprintf("stream %d: %s", e.StreamID, e.Code)
}

func (e *StreamError) Unwrap() error { return e.Cause }

// ConnectionError represents a violation that requires closing the entire
// connection. The correct response is GOAWAY followed by transport close.
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

// FrameType identifies one of the ten standard HTTP/2 frame types (RFC 9113 §11.2).
type FrameType uint8

const (
	FrameData         FrameType = 0x0
	FrameHeaders      FrameType = 0x1
	FramePriority     FrameType = 0x2
	FrameRSTStream    FrameType = 0x3
	FrameSettings     FrameType = 0x4
	FramePushPromise  FrameType = 0x5
	FramePing         FrameType = 0x6
	FrameGoAway       FrameType = 0x7
	FrameWindowUpdate FrameType = 0x8
	FrameContinuation FrameType = 0x9
)

// ClassifyError maps a (frameType, streamID, errorCode, cause) tuple to either
// a *StreamError or a *ConnectionError following RFC 9113 §5.4.
//
// Rules that always produce a *ConnectionError:
//   - ErrCodeCompressionError on any stream: HPACK state is connection-scoped.
//   - Any error on a SETTINGS, PING, or GOAWAY frame: these are connection-level.
//   - Any error on stream 0: stream 0 is the connection control channel.
//
// All other errors on a non-zero stream produce a *StreamError.
func ClassifyError(ft FrameType, streamID uint32, code ErrCode, cause error) error {
	isConnErr := code == ErrCodeCompressionError ||
		ft == FrameSettings ||
		ft == FramePing ||
		ft == FrameGoAway ||
		streamID == 0

	if isConnErr {
		return &ConnectionError{Code: code, Cause: cause}
	}
	return &StreamError{StreamID: streamID, Code: code, Cause: cause}
}
```

### The runnable demo

The demo classifies the two canonical cases — an HPACK failure that is always fatal, and a per-stream cancel that is not — and prints both, then shows the hex fallback for an unknown code.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/error-classification"
)

func main() {
	// HPACK decompression failure on stream 5 is a connection error.
	connErr := errclass.ClassifyError(
		errclass.FrameHeaders, 5, errclass.ErrCodeCompressionError, nil)
	fmt.Println(connErr)

	// A DATA frame on stream 7 with CANCEL is a stream error.
	streamErr := errclass.ClassifyError(
		errclass.FrameData, 7, errclass.ErrCodeCancel, nil)
	fmt.Println(streamErr)

	// A FRAME_SIZE_ERROR on a SETTINGS frame is always a connection error.
	settingsErr := errclass.ClassifyError(
		errclass.FrameSettings, 0, errclass.ErrCodeFrameSizeError, nil)
	fmt.Println(settingsErr)

	// An unknown code still prints, with a hex fallback.
	fmt.Println(errclass.ErrCode(0xff))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
connection error COMPRESSION_ERROR
stream 7: CANCEL
connection error FRAME_SIZE_ERROR
UNKNOWN_ERROR_CODE_0xff
```

### Tests

The tests pin every rule. `TestErrCodeString` checks the name of all fourteen codes plus the unknown fallback. The four `TestClassifyError*` functions cover the three connection-error triggers (compression code, stream 0, SETTINGS/PING/GOAWAY frame) and the stream-error default. The two unwrap tests prove `errors.Is` reaches an underlying cause through both wrappers. `ExampleClassifyError` is auto-verified by `go test`.

Create `errclass_test.go`:

```go
package errclass

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrCodeString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code ErrCode
		want string
	}{
		{ErrCodeNoError, "NO_ERROR"},
		{ErrCodeProtocolError, "PROTOCOL_ERROR"},
		{ErrCodeInternalError, "INTERNAL_ERROR"},
		{ErrCodeFlowControlError, "FLOW_CONTROL_ERROR"},
		{ErrCodeSettingsTimeout, "SETTINGS_TIMEOUT"},
		{ErrCodeStreamClosed, "STREAM_CLOSED"},
		{ErrCodeFrameSizeError, "FRAME_SIZE_ERROR"},
		{ErrCodeRefusedStream, "REFUSED_STREAM"},
		{ErrCodeCancel, "CANCEL"},
		{ErrCodeCompressionError, "COMPRESSION_ERROR"},
		{ErrCodeConnectError, "CONNECT_ERROR"},
		{ErrCodeEnhanceYourCalm, "ENHANCE_YOUR_CALM"},
		{ErrCodeInadequateSecurity, "INADEQUATE_SECURITY"},
		{ErrCodeHTTP11Required, "HTTP_1_1_REQUIRED"},
		{ErrCode(0xff), "UNKNOWN_ERROR_CODE_0xff"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.code.String(); got != tc.want {
				t.Errorf("ErrCode(0x%x).String() = %q, want %q", uint32(tc.code), got, tc.want)
			}
		})
	}
}

func TestClassifyErrorCompressionIsAlwaysConnection(t *testing.T) {
	t.Parallel()
	// HPACK state is connection-scoped: compression errors are always fatal.
	for _, streamID := range []uint32{0, 1, 3, 999} {
		err := ClassifyError(FrameHeaders, streamID, ErrCodeCompressionError, nil)
		var connErr *ConnectionError
		if !errors.As(err, &connErr) {
			t.Errorf("streamID %d: expected *ConnectionError, got %T: %v", streamID, err, err)
			continue
		}
		if connErr.Code != ErrCodeCompressionError {
			t.Errorf("streamID %d: code = %s, want COMPRESSION_ERROR", streamID, connErr.Code)
		}
	}
}

func TestClassifyErrorStream0IsAlwaysConnection(t *testing.T) {
	t.Parallel()
	err := ClassifyError(FrameData, 0, ErrCodeProtocolError, nil)
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Fatalf("stream 0 DATA: expected *ConnectionError, got %T", err)
	}
}

func TestClassifyErrorSettingsPingGoawayAreConnection(t *testing.T) {
	t.Parallel()
	for _, ft := range []FrameType{FrameSettings, FramePing, FrameGoAway} {
		err := ClassifyError(ft, 1, ErrCodeFrameSizeError, nil)
		var connErr *ConnectionError
		if !errors.As(err, &connErr) {
			t.Errorf("frame 0x%x: expected *ConnectionError, got %T", ft, err)
		}
	}
}

func TestClassifyErrorNonZeroStreamIsStream(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ft       FrameType
		streamID uint32
		code     ErrCode
	}{
		{FrameData, 1, ErrCodeCancel},
		{FrameHeaders, 7, ErrCodeProtocolError},
		{FrameWindowUpdate, 3, ErrCodeFlowControlError},
	}
	for _, tc := range cases {
		err := ClassifyError(tc.ft, tc.streamID, tc.code, nil)
		var streamErr *StreamError
		if !errors.As(err, &streamErr) {
			t.Errorf("(%v, %d, %v): expected *StreamError, got %T", tc.ft, tc.streamID, tc.code, err)
			continue
		}
		if streamErr.StreamID != tc.streamID || streamErr.Code != tc.code {
			t.Errorf("streamErr = %+v, want {StreamID:%d Code:%v}", streamErr, tc.streamID, tc.code)
		}
	}
}

func TestStreamErrorUnwrapsCause(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("sentinel")
	err := ClassifyError(FrameData, 5, ErrCodeCancel, sentinel)
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false; err = %v", err)
	}
}

func TestConnectionErrorUnwrapsCause(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("hpack broken")
	err := ClassifyError(FrameHeaders, 1, ErrCodeCompressionError, sentinel)
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false; err = %v", err)
	}
}

func ExampleClassifyError() {
	// HPACK decompression failure on any stream is a connection error.
	connErr := ClassifyError(FrameHeaders, 5, ErrCodeCompressionError, nil)
	fmt.Println(connErr)

	// A DATA frame on a specific stream with CANCEL is a stream error.
	streamErr := ClassifyError(FrameData, 7, ErrCodeCancel, nil)
	fmt.Println(streamErr)

	// Output:
	// connection error COMPRESSION_ERROR
	// stream 7: CANCEL
}
```

## Review

The classifier is correct when its disposition is driven only by the four rules and nothing else. The most damaging mistake is treating a compression error as stream-scoped: the test sweeps stream IDs 0, 1, 3, and 999 with `ErrCodeCompressionError` and demands a `*ConnectionError` for every one, because the HPACK context is shared and a `RST_STREAM` response would leave it corrupted. The second mistake is forgetting that any error on stream 0, or on a SETTINGS/PING/GOAWAY frame, is connection-level regardless of code. Branch on the result with `errors.As`, not on a string match, and let both wrappers carry their cause through `Unwrap` so the original decoder error survives. The classifier holds no state, so the race detector has nothing to find here — but running the suite under `-race` is still the habit to keep across every module in this lesson.

## Resources

- [RFC 9113 §5.4 — Error Handling](https://httpwg.org/specs/rfc9113.html#ErrorHandler) — the authoritative split between stream errors and connection errors and the RST_STREAM/GOAWAY response rules.
- [RFC 9113 §7 — Error Codes](https://httpwg.org/specs/rfc9113.html#ErrorCodes) — the full 32-bit error-code registry these constants mirror.
- [errors.As](https://pkg.go.dev/errors#As) — the type-directed unwrap callers use to branch on stream vs. connection disposition.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-settings-negotiation.md](02-settings-negotiation.md)
