# Exercise 4: Config Decoding: json.Unmarshaler Must Have a Pointer Receiver

Config files are full of human-friendly durations like `"30s"`. Decoding them into
a `time.Duration` field requires a custom `UnmarshalJSON` — and that method *must*
have a pointer receiver, or the field is silently left at zero with no error. This
module builds the `Duration` type the right way and demonstrates the silent-drop
failure of the wrong way in a test.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
cfgdur/                     independent module: example.com/cfgdur
  go.mod                    go 1.25
  duration.go               type Duration; UnmarshalJSON (ptr recv), MarshalJSON (val recv)
  cmd/
    demo/
      main.go               decode a config, print, re-encode
  duration_test.go          round-trip, invalid input error, value-receiver silent-drop proof
```

- Files: `duration.go`, `cmd/demo/main.go`, `duration_test.go`.
- Implement: a `Duration` wrapping `time.Duration`, with `UnmarshalJSON` on a pointer receiver (parses `"30s"`) and `MarshalJSON` on a value receiver (emits `"30s"`).
- Test: round-trip a config struct; assert an invalid duration returns an error; a subtest proves that a value-receiver `UnmarshalJSON` leaves the field at zero.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cfgdur/cmd/demo
cd ~/go-exercises/cfgdur
go mod init example.com/cfgdur
go mod edit -go=1.25
```

### Why UnmarshalJSON needs a pointer receiver and MarshalJSON does not

`json.Unmarshal` writes *into* its destination. For a struct field, it takes the
field's address and looks for `UnmarshalJSON` on that pointer. `UnmarshalJSON`
therefore must mutate `*d` — it needs a **pointer** receiver. The failure mode when
you get this wrong is not a compile error and not a runtime error: it is silent.
If `UnmarshalJSON` has a *value* receiver, the method is still promoted into
`*Duration`'s method set, so json still calls it, but the call receives a **copy**
of the field. The method parses the string and stores it into the copy; the copy
is discarded when the method returns; the real field stays at its zero value. A
config that says `"timeout": "30s"` ends up with a zero timeout, and nothing tells
you. This module proves that behavior with a `badDuration` type in the test.

`MarshalJSON` only reads the receiver, so it takes a **value** receiver by
convention. A value-receiver method is in the method set of both `Duration` and
`*Duration`, so both a `Duration` value field and a `*Duration` field marshal
correctly. This is the standard split: value receiver for the read side, pointer
receiver for the write side.

Create `duration.go`:

```go
// duration.go
package cfgdur

import (
	"encoding/json"
	"time"
)

// Duration wraps time.Duration so it decodes from a human string like "30s" in
// JSON config. UnmarshalJSON has a pointer receiver (it mutates the target);
// MarshalJSON has a value receiver (it only reads).
type Duration time.Duration

// Compile-time contracts: *Duration decodes, Duration (and thus *Duration)
// encodes.
var (
	_ json.Unmarshaler = (*Duration)(nil)
	_ json.Marshaler   = Duration(0)
)

// UnmarshalJSON parses a JSON string such as "30s" into the Duration. It has a
// pointer receiver so the parsed value persists in the destination field.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON emits the duration as a JSON string like "30s". A value receiver
// means both Duration and *Duration marshal.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Std returns the wrapped time.Duration for use with time APIs.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is a small server config using Duration fields.
type Config struct {
	Timeout   Duration `json:"timeout"`
	KeepAlive Duration `json:"keep_alive"`
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/cfgdur"
)

func main() {
	raw := []byte(`{"timeout":"30s","keep_alive":"1m30s"}`)

	var cfg cfgdur.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Println("decode error:", err)
		return
	}
	fmt.Printf("timeout   = %s\n", cfg.Timeout.Std())
	fmt.Printf("keepalive = %s\n", cfg.KeepAlive.Std())

	out, _ := json.Marshal(cfg)
	fmt.Printf("re-encoded = %s\n", out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
timeout   = 30s
keepalive = 1m30s
re-encoded = {"timeout":"30s","keep_alive":"1m30s"}
```

### Tests

`TestRoundTrip` decodes a config and re-encodes it, asserting both directions.
`TestInvalidDuration` asserts a bad string surfaces an error (not a silent zero).
`TestValueReceiverSilentlyDrops` is the teaching test: `badDuration` has a
value-receiver `UnmarshalJSON`, json calls it, but the field stays zero — proving
exactly why the real `Duration` uses a pointer receiver.

Create `duration_test.go`:

```go
// duration_test.go
package cfgdur

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	var cfg Config
	if err := json.Unmarshal([]byte(`{"timeout":"30s","keep_alive":"90s"}`), &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Timeout.Std() != 30*time.Second {
		t.Fatalf("Timeout = %s, want 30s", cfg.Timeout.Std())
	}
	if cfg.KeepAlive.Std() != 90*time.Second {
		t.Fatalf("KeepAlive = %s, want 1m30s", cfg.KeepAlive.Std())
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{"timeout":"30s","keep_alive":"1m30s"}`
	if string(out) != want {
		t.Fatalf("Marshal = %s, want %s", out, want)
	}
}

func TestInvalidDuration(t *testing.T) {
	t.Parallel()

	var cfg Config
	err := json.Unmarshal([]byte(`{"timeout":"not-a-duration"}`), &cfg)
	if err == nil {
		t.Fatal("Unmarshal of invalid duration returned nil error, want a parse error")
	}
}

// badDuration reproduces the classic bug: UnmarshalJSON on a VALUE receiver.
type badDuration time.Duration

func (d badDuration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d = badDuration(parsed) // writes to the COPY; lost when the method returns
	_ = d
	return nil
}

type badConfig struct {
	Timeout badDuration `json:"timeout"`
}

func TestValueReceiverSilentlyDrops(t *testing.T) {
	t.Parallel()

	var cfg badConfig
	if err := json.Unmarshal([]byte(`{"timeout":"30s"}`), &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// json called UnmarshalJSON, but the value receiver mutated a copy, so the
	// field is silently left at zero. This is the bug the real Duration avoids.
	if time.Duration(cfg.Timeout) != 0 {
		t.Fatalf("value-receiver Timeout = %s, expected the silent-drop zero", time.Duration(cfg.Timeout))
	}
}
```

## Review

The decoder is correct when a `"30s"` string round-trips to `30*time.Second` and
back, and an invalid string returns an error rather than a zero. The pointer
receiver on `UnmarshalJSON` is the whole point: `TestValueReceiverSilentlyDrops`
demonstrates the alternative — json *does* call a value-receiver `UnmarshalJSON`,
so there is no error to catch, yet the field is zero. That silence is what makes
the bug dangerous in production: a mis-declared receiver turns a 30-second timeout
into an instant one with no signal. The `var _ json.Unmarshaler = (*Duration)(nil)`
contract documents that it is `*Duration`, not `Duration`, that decodes.

## Resources

- [encoding/json: Unmarshaler](https://pkg.go.dev/encoding/json#Unmarshaler) — the interface json calls, and that it needs an addressable target.
- [encoding/json: Marshaler](https://pkg.go.dev/encoding/json#Marshaler) — the value-receiver read side.
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — the string grammar `Duration` parses.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-http-handler-pointer-receiver.md](03-http-handler-pointer-receiver.md) | Next: [05-typed-nil-error-interface.md](05-typed-nil-error-interface.md)
