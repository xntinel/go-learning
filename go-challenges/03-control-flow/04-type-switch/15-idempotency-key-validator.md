# Exercise 15: Validate and Normalize Idempotency Keys Across API Clients

**Nivel: Intermedio** — validacion rapida (un test corto).

An API gateway accepts an `Idempotency-Key` from every client that wants
at-most-once processing for a request, but clients disagree on encoding: one
SDK sends a dash-formatted UUID string, another sends the same 16 bytes
base64-encoded, and a legacy integration sends a plain incrementing counter.
The dedup cache is only useful if all three collapse to the same lookup key
when they represent the same logical request — otherwise a base64-encoded
retry of a UUID-string request is treated as a brand-new request and
processed twice.

## What you'll build

```text
idempotency-key-validator/   independent module: example.com/idempotency-key-validator
  go.mod                     go 1.24
  idemkey.go                 Normalize(v any) (string, error)
  cmd/
    demo/
      main.go                normalizes a mix of UUID, base64, and numeric keys
  idemkey_test.go             table test over every encoding plus rejected shapes
```

- Files: `idemkey.go`, `cmd/demo/main.go`, `idemkey_test.go`.
- Implement: `Normalize(v any) (string, error)`, type-switching on `string`,
  `int64`, and `float64` to produce one canonical `uuid:` or `num:` key.
- Test: a dash-formatted UUID, the same UUID uppercase, the same UUID
  base64-encoded (all three must produce the identical canonical string), a
  positive `int64` counter, a negative counter rejected, an integral
  `float64` counter, a non-integral `float64` rejected, a malformed string,
  and an unsupported type.

Set up the module:

```bash
mkdir -p ~/go-exercises/idempotency-key-validator/cmd/demo
cd ~/go-exercises/idempotency-key-validator
go mod init example.com/idempotency-key-validator
go mod edit -go=1.24
```

The gateway's dedup cache is only as good as the key it indexes by. If the
same logical UUID arrives once dash-formatted and once base64-encoded — both
legitimate on the wire, since some SDKs marshal UUIDs as raw bytes — the
cache must produce identical keys for both or the second delivery is treated
as new work. The type switch is what lets `Normalize` recognize which
encoding it received and route to the matching canonicalization: a
dash-formatted string is validated against a UUID regex and lowercased in
place; a base64 string is decoded first and only then rendered through the
same canonical formatter, so both paths converge on one code path
(`formatUUID`) for the actual byte-to-string rendering. Numeric keys take a
completely different shape (no UUID structure at all), so they get their own
prefix (`num:`) rather than being forced through UUID formatting — a `float64`
must additionally be checked for integral value, since a fractional numeric
key is not a legitimate counter and silently truncating it would collide two
different keys.

Create `idemkey.go`:

```go
package idemkey

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalidKey is the sentinel for an idempotency key that cannot be
// normalized into the gateway's canonical form.
var ErrInvalidKey = errors.New("invalid idempotency key")

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Normalize converts a client-supplied idempotency key, decoded from JSON or
// a header into `any`, into one canonical string the gateway uses as a
// dedup-cache lookup key. Clients disagree on encoding: some send a
// dash-formatted UUID string, some send a base64-encoded 16-byte UUID, and
// legacy clients send a plain numeric counter. All three must collapse to a
// canonical form so that the same logical request, sent twice in different
// encodings, hits the same cache entry.
func Normalize(v any) (string, error) {
	switch k := v.(type) {
	case string:
		if uuidPattern.MatchString(k) {
			return "uuid:" + lowerHex(k), nil
		}
		if b, err := base64.StdEncoding.DecodeString(k); err == nil && len(b) == 16 {
			return "uuid:" + formatUUID(b), nil
		}
		return "", fmt.Errorf("%w: string %q is neither a UUID nor base64-encoded 16 bytes", ErrInvalidKey, k)
	case int64:
		if k < 0 {
			return "", fmt.Errorf("%w: negative counter %d", ErrInvalidKey, k)
		}
		return fmt.Sprintf("num:%d", k), nil
	case float64:
		if k < 0 || k != float64(int64(k)) {
			return "", fmt.Errorf("%w: non-integral or negative numeric key %v", ErrInvalidKey, k)
		}
		return fmt.Sprintf("num:%d", int64(k)), nil
	default:
		return "", fmt.Errorf("%w: unsupported type %T", ErrInvalidKey, v)
	}
}

// formatUUID renders 16 raw bytes as the canonical dash-grouped, lowercase
// hex UUID string.
func formatUUID(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// lowerHex canonicalizes an already dash-formatted UUID string to lowercase,
// since the same UUID sent with uppercase hex digits must map to the same
// cache key as its lowercase form.
func lowerHex(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'F' {
			out[i] = c - 'A' + 'a'
		}
	}
	return string(out)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/idempotency-key-validator"
)

func main() {
	keys := []any{
		"550e8400-e29b-41d4-a716-446655440000",
		"550E8400-E29B-41D4-A716-446655440000",
		int64(42),
		42.0,
	}
	for _, k := range keys {
		canonical, err := idemkey.Normalize(k)
		if err != nil {
			log.Printf("reject %v: %v", k, err)
			continue
		}
		fmt.Printf("%v -> %s\n", k, canonical)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
550e8400-e29b-41d4-a716-446655440000 -> uuid:550e8400-e29b-41d4-a716-446655440000
550E8400-E29B-41D4-A716-446655440000 -> uuid:550e8400-e29b-41d4-a716-446655440000
42 -> num:42
42 -> num:42
```

### Tests

The base64 test vector is derived at runtime from the same hex digits as the
dash-formatted UUID case, rather than hand-typed, so the test proves the two
encodings genuinely converge instead of coincidentally matching a
copy-pasted literal.

Create `idemkey_test.go`:

```go
package idemkey

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
)

func TestNormalize(t *testing.T) {
	t.Parallel()

	rawHex := "550e8400e29b41d4a716446655440000"
	rawBytes, err := hex.DecodeString(rawHex)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(rawBytes)
	wantUUID := "uuid:550e8400-e29b-41d4-a716-446655440000"

	tests := []struct {
		name    string
		value   any
		want    string
		wantErr bool
	}{
		{"dash uuid lowercase", "550e8400-e29b-41d4-a716-446655440000", wantUUID, false},
		{"dash uuid uppercase canonicalizes to lowercase", "550E8400-E29B-41D4-A716-446655440000", wantUUID, false},
		{"base64 uuid matches dash form", b64, wantUUID, false},
		{"int64 counter", int64(42), "num:42", false},
		{"negative int64 rejected", int64(-1), "", true},
		{"float64 integral counter", 42.0, "num:42", false},
		{"float64 non-integral rejected", 42.5, "", true},
		{"malformed string rejected", "not-a-key", "", true},
		{"unsupported bool type rejected", true, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Normalize(tt.value)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidKey) {
					t.Fatalf("Normalize(%v) err = %v, want ErrInvalidKey", tt.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%v) unexpected error: %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("Normalize(%v) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The two UUID paths (dash-formatted and base64) are correct only because both
funnel through the same `formatUUID` renderer — the type switch's job is
just to get from whichever wire encoding arrived to a `[]byte`, not to
duplicate the formatting logic per encoding. The most common way to break
this is to canonicalize the dash-formatted case with a different code path
than the base64 case (say, a naive `strings.ToLower` applied only to the
regex branch): the two would drift apart the moment either formatting rule
changes. The `float64` branch's integral check is the other detail worth
protecting — dropping it would silently truncate a fractional key into a
valid-looking but wrong counter value.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [RFC 4122: A Universally Unique Identifier (UUID) URN Namespace](https://www.rfc-editor.org/rfc/rfc4122)
- [encoding/base64](https://pkg.go.dev/encoding/base64)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-metrics-label-sanitizer.md](14-metrics-label-sanitizer.md) | Next: [16-batch-request-unpacker.md](16-batch-request-unpacker.md)
