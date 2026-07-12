# Exercise 25: Route Protocol Versions With Fallthrough and Init-Statement

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A service that has shipped three protocol revisions still has to talk to
clients running any of them, and the graceful way to handle a client that
claims a newer version but can't actually negotiate its features is to
fall back to the decoder for the version below it — not to reject the
message outright. This module builds that router with a switch-with-init
that normalizes the version tag in the same statement that dispatches on
it, and a fallback chain built from `fallthrough` statements that are only
*reached* when the current version's negotiation fails. It is
self-contained: its own `go mod init`, code, demo, and test.

## What you'll build

```text
protoversion/                independent module: example.com/protocol-version-router-with-cascading-fallback
  go.mod                      go 1.24
  protoversion.go               package protoversion; ErrUnknownVersion; Message; Decode(msg) (result, usedVersion string, err error)
  cmd/demo/main.go              runnable demo over seven messages spanning every fallback path
  protoversion_test.go          a version-compatibility matrix, an unknown version, and a payload-content check
```

- Implement: `Decode(msg Message) (result, usedVersion string, err error)` — a switch with an init-statement normalizing the version tag, whose `v3` and `v2` cases each try their own decoder first and only reach `fallthrough` when negotiation fails.
- Test: a compatibility matrix covering every arrival version crossed with every relevant feature-support combination, an unrecognized version, and a check that the decoded result actually carries the payload through.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A fallthrough you only reach conditionally

The concepts file is emphatic that `fallthrough` is unconditional: once
control reaches the statement, it jumps into the next case body no matter
what, without re-testing that case's own condition. This exercise doesn't
contradict that — it uses the one pattern compatible with it: the
condition lives *before* the `fallthrough`, as a plain `if` that returns
early on success. `case "v3":` tries `msg.SupportsV3Features`; if that's
true, `Decode` returns right there and the `fallthrough` statement below it
never executes. Only when negotiation fails does control reach
`fallthrough`, which then unconditionally enters `case "v2":`'s body — and
that body repeats the same pattern one level down. This is exactly why a
v2 message arriving *directly* behaves identically to a v3 message that
fell back to v2: execution enters `case "v2":` either by matching it
directly or by falling into it from above, and from that point on the two
paths are indistinguishable, which is the property the compatibility
matrix test is built to confirm.

The switch-with-init (`switch version := normalizeVersion(msg.Version);
version`) keeps `version` — the normalized tag — scoped to the switch and
its cases, exactly where it belongs: nothing outside `Decode` needs it, and
nothing before the switch needs the raw, un-normalized `msg.Version`
anymore once the switch begins.

Create `protoversion.go`:

```go
// Package protoversion routes a tagged protocol message to the newest
// decoder it can actually use, falling back to older decoders when the
// message doesn't support a newer version's features. The version is
// normalized in a switch init-statement, and the fallback chain is a
// genuine, conditionally-reached fallthrough.
package protoversion

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownVersion marks a version tag outside the closed v1/v2/v3 set.
var ErrUnknownVersion = errors.New("protoversion: unknown protocol version")

// Message is an incoming message tagged with a protocol version and the
// newer feature sets it happens to support. A real message would carry its
// own capability flags negotiated at connection time; this exercise models
// them as plain booleans so the version-compatibility matrix is explicit.
type Message struct {
	Version            string
	SupportsV3Features bool
	SupportsV2Features bool
	Payload            string
}

// Decode resolves the newest decoder Message.Version's feature support
// allows, and reports which version actually decoded it. A v3 message
// that can't negotiate v3 features falls back to v2, and a v2 message
// (whether it arrived as v2 directly, or fell back from v3) that can't
// negotiate v2 features falls back to v1, which is the floor: v1 has no
// optional features left to fail.
func Decode(msg Message) (result, usedVersion string, err error) {
	switch version := normalizeVersion(msg.Version); version {
	case "v3":
		if msg.SupportsV3Features {
			return fmt.Sprintf("decoded-v3:%s", msg.Payload), "v3", nil
		}
		fallthrough
	case "v2":
		if msg.SupportsV2Features {
			return fmt.Sprintf("decoded-v2:%s", msg.Payload), "v2", nil
		}
		fallthrough
	case "v1":
		return fmt.Sprintf("decoded-v1:%s", msg.Payload), "v1", nil
	default:
		return "", "", fmt.Errorf("%w: %q", ErrUnknownVersion, version)
	}
}

// normalizeVersion lowercases and trims the raw version tag, and adds a
// leading "v" if the caller sent a bare number ("3" becomes "v3"), so the
// switch in Decode only ever has to match exactly "v1", "v2", or "v3".
func normalizeVersion(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v != "" && !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	protoversion "example.com/protocol-version-router-with-cascading-fallback"
)

func main() {
	messages := []protoversion.Message{
		{Version: "v3", SupportsV3Features: true, SupportsV2Features: true, Payload: "hello"},
		{Version: "v3", SupportsV3Features: false, SupportsV2Features: true, Payload: "hello"},
		{Version: "v3", SupportsV3Features: false, SupportsV2Features: false, Payload: "hello"},
		{Version: " V2 ", SupportsV2Features: true, Payload: "world"},
		{Version: "2", SupportsV2Features: false, Payload: "world"},
		{Version: "v1", Payload: "bare"},
		{Version: "v9", Payload: "future"},
	}

	for _, msg := range messages {
		result, used, err := protoversion.Decode(msg)
		if err != nil {
			fmt.Printf("version=%-6q -> error: %v\n", msg.Version, err)
			continue
		}
		fmt.Printf("version=%-6q -> used=%s result=%s\n", msg.Version, used, result)
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
version="v3"   -> used=v3 result=decoded-v3:hello
version="v3"   -> used=v2 result=decoded-v2:hello
version="v3"   -> used=v1 result=decoded-v1:hello
version=" V2 " -> used=v2 result=decoded-v2:world
version="2"    -> used=v1 result=decoded-v1:world
version="v1"   -> used=v1 result=decoded-v1:bare
version="v9"   -> error: protoversion: unknown protocol version: "v9"
```

### Tests

`TestDecodeCompatibilityMatrix` is exactly that: a matrix crossing arrival
version against feature support, covering v3 falling back zero, one, and
two levels; v2 arriving directly with and without its own feature support;
v1 as the unconditional floor; and two normalization cases (whitespace
plus case, and a bare numeric shorthand). `TestDecodeUnknownVersion` checks
the fail-closed `default` branch, and `TestDecodeResultIncludesPayload`
confirms the decoded string actually carries the original payload through,
not just a version label.

Create `protoversion_test.go`:

```go
package protoversion

import (
	"errors"
	"testing"
)

// TestDecodeCompatibilityMatrix drives every combination of arrival version
// and feature support that matters: whether v3 falls back to v2, whether
// v2 (arriving directly or via fallback from v3) falls back to v1, and
// that v1 is always the floor.
func TestDecodeCompatibilityMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  Message
		want string
	}{
		{
			name: "v3 with full feature support decodes as v3",
			msg:  Message{Version: "v3", SupportsV3Features: true, SupportsV2Features: true, Payload: "hi"},
			want: "v3",
		},
		{
			name: "v3 without v3 features falls back to v2",
			msg:  Message{Version: "v3", SupportsV3Features: false, SupportsV2Features: true, Payload: "hi"},
			want: "v2",
		},
		{
			name: "v3 without any negotiable features falls all the way to v1",
			msg:  Message{Version: "v3", SupportsV3Features: false, SupportsV2Features: false, Payload: "hi"},
			want: "v1",
		},
		{
			name: "v2 arriving directly with v2 support decodes as v2",
			msg:  Message{Version: "v2", SupportsV2Features: true, Payload: "hi"},
			want: "v2",
		},
		{
			name: "v2 arriving directly without v2 support falls back to v1",
			msg:  Message{Version: "v2", SupportsV2Features: false, Payload: "hi"},
			want: "v1",
		},
		{
			name: "v1 arriving directly always decodes as v1",
			msg:  Message{Version: "v1", Payload: "hi"},
			want: "v1",
		},
		{
			name: "normalization: whitespace and case",
			msg:  Message{Version: " V3 ", SupportsV3Features: true, Payload: "hi"},
			want: "v3",
		},
		{
			name: "normalization: bare numeric shorthand",
			msg:  Message{Version: "3", SupportsV3Features: true, Payload: "hi"},
			want: "v3",
		},
	}

	for _, tc := range tests {
		_, used, err := Decode(tc.msg)
		if err != nil {
			t.Errorf("%s: Decode(%+v) unexpected error: %v", tc.name, tc.msg, err)
			continue
		}
		if used != tc.want {
			t.Errorf("%s: Decode(%+v) usedVersion = %q, want %q", tc.name, tc.msg, used, tc.want)
		}
	}
}

func TestDecodeUnknownVersion(t *testing.T) {
	t.Parallel()

	_, _, err := Decode(Message{Version: "v9", Payload: "hi"})
	if !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("Decode(v9) error = %v, want errors.Is match for ErrUnknownVersion", err)
	}
}

func TestDecodeResultIncludesPayload(t *testing.T) {
	t.Parallel()

	result, _, err := Decode(Message{Version: "v3", SupportsV3Features: true, Payload: "payload-123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "decoded-v3:payload-123"
	if result != want {
		t.Errorf("result = %q, want %q", result, want)
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The router is correct when a v3 message that fails v3 negotiation but
supports v2 features lands on v2 (not v1), when a v2 message arriving
directly behaves identically to one that fell back from v3, when v1 is
always reachable as the unconditional floor, and when an unrecognized
version fails closed instead of guessing a decoder. Carry this forward:
`fallthrough`'s unconditional jump is compatible with conditional fallback
logic as long as the condition is checked *before* the `fallthrough`
statement with an early return on success — the keyword itself never gets
to be conditional, but reaching it can be.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the switch init-statement and the fallthrough statement.
- [gRPC: API Versioning](https://grpc.io/docs/guides/versioning/) — graceful protocol version negotiation in a real RPC framework.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-lease-renewal-backoff-strategy.md](24-lease-renewal-backoff-strategy.md) | Next: [26-sliding-window-rate-limiter.md](26-sliding-window-rate-limiter.md)
