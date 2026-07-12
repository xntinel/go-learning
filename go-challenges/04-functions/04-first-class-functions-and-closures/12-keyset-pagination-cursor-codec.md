# Exercise 12: Keyset-Pagination Cursor Codec as a Closure Pair

**Nivel: Intermedio** — validacion rapida (un test corto).

A keyset-paginated API endpoint hands clients an opaque cursor instead of a
page number, and must reject a cursor a client edited by hand or copied from
a different tenant. `NewCodec` returns two closures — `encode` and `decode`
— that share one captured salt, never exposed outside them, which is what
makes tampering and cross-tenant reuse detectable.

## What you'll build

```text
keysetcursor/              independent module: example.com/keyset-cursor-codec
  go.mod                   go 1.24
  cursor.go                NewCodec returns (encode, decode) sharing a salt
  cursor_test.go           table test: round trip, tamper, cross-salt, garbage
```

- Files: `cursor.go`, `cursor_test.go`.
- Implement: `NewCodec(salt string) (encode func(lastID int64, lastCreatedAt time.Time) string, decode func(cursor string) (int64, time.Time, error))`, where both closures capture a `checksum` helper built over `salt`.
- Test: a table round-trips a cursor, then checks a tampered cursor, a cursor decoded with a different salt, and outright garbage all return `ErrInvalidCursor`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/12-keyset-pagination-cursor-codec
cd go-solutions/04-functions/04-first-class-functions-and-closures/12-keyset-pagination-cursor-codec
go mod edit -go=1.24
```

### One captured salt, two closures

`NewCodec` builds a local `checksum` closure over `salt` and shares it between
`encode` and `decode` by defining both inside the same call. `encode` packs
`lastID` and `lastCreatedAt` into a `"id:nanos"` payload, appends
`checksum(payload)`, and base64-encodes the result — that whole string is the
opaque cursor a client stores and echoes back on the next request. `decode`
reverses the steps and recomputes the checksum itself instead of trusting the
one embedded in the cursor; if they disagree — because a byte was edited, or
because the cursor was produced by a codec built with a different salt — it
returns `ErrInvalidCursor` instead of a corrupted id or timestamp. Neither
closure exposes `salt` or `checksum` to the caller; they exist only inside
this one captured environment, which is exactly what keeps two `NewCodec`
calls with different salts from being able to decode each other's cursors.

Create `cursor.go`:

```go
package keysetcursor

import (
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidCursor is returned when a cursor is malformed or fails the
// checksum, including a cursor produced by a codec built with a different
// salt.
var ErrInvalidCursor = errors.New("keysetcursor: invalid cursor")

// NewCodec returns an encode/decode closure pair for opaque keyset-pagination
// cursors. Both closures share one captured salt: encode mixes it into a
// checksum, decode recomputes the checksum and rejects any cursor whose
// checksum does not match — including one produced by a codec built with a
// different salt. The salt itself is never exposed outside the two closures.
func NewCodec(salt string) (
	encode func(lastID int64, lastCreatedAt time.Time) string,
	decode func(cursor string) (int64, time.Time, error),
) {
	checksum := func(payload string) string {
		sum := crc32.ChecksumIEEE([]byte(salt + payload))
		return strconv.FormatUint(uint64(sum), 16)
	}

	encode = func(lastID int64, lastCreatedAt time.Time) string {
		payload := fmt.Sprintf("%d:%d", lastID, lastCreatedAt.UnixNano())
		raw := payload + ":" + checksum(payload)
		return base64.RawURLEncoding.EncodeToString([]byte(raw))
	}

	decode = func(cursor string) (int64, time.Time, error) {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
		}

		parts := strings.Split(string(raw), ":")
		if len(parts) != 3 {
			return 0, time.Time{}, ErrInvalidCursor
		}

		payload := parts[0] + ":" + parts[1]
		if checksum(payload) != parts[2] {
			return 0, time.Time{}, ErrInvalidCursor
		}

		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, time.Time{}, ErrInvalidCursor
		}
		nanos, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, time.Time{}, ErrInvalidCursor
		}

		return id, time.Unix(0, nanos).UTC(), nil
	}

	return encode, decode
}
```

### Tests

Create `cursor_test.go`:

```go
package keysetcursor

import (
	"errors"
	"testing"
	"time"
)

func TestCodecRoundTrip(t *testing.T) {
	encode, decode := NewCodec("pepper")
	want := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	cursor := encode(42, want)
	id, createdAt, err := decode(cursor)
	if err != nil {
		t.Fatalf("decode() error = %v, want nil", err)
	}
	if id != 42 {
		t.Fatalf("decode() id = %d, want 42", id)
	}
	if !createdAt.Equal(want) {
		t.Fatalf("decode() createdAt = %v, want %v", createdAt, want)
	}
}

func TestCodecRejectsTamperedCursor(t *testing.T) {
	encode, decode := NewCodec("pepper")
	cursor := encode(42, time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))

	tampered := []rune(cursor)
	tampered[0] = tampered[0] + 1
	_, _, err := decode(string(tampered))
	if !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("decode(tampered) error = %v, want ErrInvalidCursor", err)
	}
}

func TestCodecRejectsCrossSaltCursor(t *testing.T) {
	encodeA, _ := NewCodec("pepper")
	_, decodeB := NewCodec("salt")

	cursor := encodeA(7, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, _, err := decodeB(cursor); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("decodeB(cursor from codec A) error = %v, want ErrInvalidCursor", err)
	}
}

func TestCodecRejectsGarbageCursor(t *testing.T) {
	_, decode := NewCodec("pepper")
	if _, _, err := decode("not-a-real-cursor!!"); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("decode(garbage) error = %v, want ErrInvalidCursor", err)
	}
}
```

Verify: `go test -count=1 ./...`

## Review

Two closures created in the same `NewCodec` call share one captured `salt`
and the `checksum` helper built over it, without either value ever leaving
that environment through the return signature. The round-trip test proves the
happy path; the tamper, cross-salt, and garbage tests prove `decode` refuses
anything it did not itself produce with a matching salt, instead of silently
returning a wrong id. This is the same "closures as private state" idea as
the rest of the lesson, applied to a pair of complementary functions instead
of one.

## Resources

- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — how the two closures share the captured `salt`.
- [pkg.go.dev: encoding/base64](https://pkg.go.dev/encoding/base64) — `RawURLEncoding` for compact, URL-safe opaque tokens.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-token-bucket-closure.md](11-token-bucket-closure.md) | Next: [13-request-scoped-logger-factory.md](13-request-scoped-logger-factory.md)
