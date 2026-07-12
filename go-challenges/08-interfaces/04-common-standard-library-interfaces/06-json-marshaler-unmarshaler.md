# Exercise 6: Custom json.Marshaler/Unmarshaler for an API Timestamp and Enum

Every API DTO shapes its wire format at the JSON boundary. Two of the most common
shaping needs: a timestamp that goes over the wire as epoch milliseconds (an
integer) instead of RFC3339, and an enum that serializes as its string name and
rejects unknown names on decode. Both are `json.Marshaler`/`json.Unmarshaler`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
apidto/                     independent module: example.com/apidto
  go.mod
  apidto.go                 EpochMillis (int wire form); OrderStatus (string wire form); Order
  cmd/
    demo/
      main.go               marshals an Order, unmarshals it back
  apidto_test.go            round-trip, wire-format assertions, unknown-enum rejection
```

- Files: `apidto.go`, `cmd/demo/main.go`, `apidto_test.go`.
- Implement: `MarshalJSON`/`UnmarshalJSON` on `EpochMillis` (an integer wire form) and on `OrderStatus` (a quoted string, rejecting unknown names on decode).
- Test: `Marshal` then `Unmarshal` round-trips; the wire format is an integer for `EpochMillis` and a quoted string for the enum; unknown enum names error on decode; a struct embedding both serializes as expected.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/04-common-standard-library-interfaces/06-json-marshaler-unmarshaler/cmd/demo
cd go-solutions/08-interfaces/04-common-standard-library-interfaces/06-json-marshaler-unmarshaler
```

### Receiver rules and the encode/decode split

`MarshalJSON() ([]byte, error)` returns the *raw* JSON for the value — you must
return valid JSON, so an integer is the literal digits and a string must be
quoted (let `json.Marshal(name)` do the quoting and escaping rather than
hand-building `"..."`). `UnmarshalJSON([]byte) error` receives the raw JSON token
and parses it.

Receivers matter. `MarshalJSON` can be on a value receiver: `encoding/json` can
call it whether it has an addressable value or a copy. `UnmarshalJSON` must be on
a *pointer* receiver — it mutates the destination — and `encoding/json` only calls
it when it has an addressable value to write into. A struct field is addressable,
so `Order.Status` decodes correctly; a value stored in a map is not, which is the
classic "my custom unmarshal never runs" bug. Because these are struct fields
here, pointer-receiver `UnmarshalJSON` fires as intended.

`EpochMillis` embeds `time.Time` for its arithmetic but overrides the JSON
methods: `MarshalJSON` emits `UnixMilli()` as decimal digits, and `UnmarshalJSON`
parses the integer back through `time.UnixMilli`. The embedded `time.Time`'s own
RFC3339 `MarshalJSON` is shadowed by the one defined directly on `EpochMillis`.
`OrderStatus` marshals through a name table and, crucially, its `UnmarshalJSON`
*rejects* a name not in the table — decoding untrusted input into a known-good set
is exactly what an API boundary is for. (`OrderStatus` also has a `String()` so
`%s` prints the name; `encoding/json` still prefers `MarshalJSON` over `Stringer`.)
The modern direction is `encoding/json/v2` with `MarshalerTo`/`UnmarshalerFrom`,
which stream into an encoder to avoid the intermediate `[]byte`; the v1 interfaces
shown here remain the everyday tool.

Create `apidto.go`:

```go
package apidto

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// EpochMillis serializes a time as an integer count of milliseconds since the
// Unix epoch, the shape many JSON APIs use instead of RFC3339.
type EpochMillis struct {
	time.Time
}

func (e EpochMillis) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(e.UnixMilli(), 10)), nil
}

func (e *EpochMillis) UnmarshalJSON(data []byte) error {
	ms, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return fmt.Errorf("epoch millis: %w", err)
	}
	e.Time = time.UnixMilli(ms).UTC()
	return nil
}

// OrderStatus serializes as its stable string name and rejects unknown names on
// decode.
type OrderStatus int

const (
	StatusUnknown OrderStatus = iota
	StatusPending
	StatusPaid
	StatusShipped
	StatusDelivered
	StatusCancelled
)

var statusNames = map[OrderStatus]string{
	StatusPending:   "pending",
	StatusPaid:      "paid",
	StatusShipped:   "shipped",
	StatusDelivered: "delivered",
	StatusCancelled: "cancelled",
}

var statusByName = func() map[string]OrderStatus {
	m := make(map[string]OrderStatus, len(statusNames))
	for k, v := range statusNames {
		m[v] = k
	}
	return m
}()

func (s OrderStatus) String() string {
	if name, ok := statusNames[s]; ok {
		return name
	}
	return "unknown"
}

func (s OrderStatus) MarshalJSON() ([]byte, error) {
	name, ok := statusNames[s]
	if !ok {
		return nil, fmt.Errorf("cannot marshal unknown order status %d", int(s))
	}
	return json.Marshal(name)
}

func (s *OrderStatus) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	v, ok := statusByName[name]
	if !ok {
		return fmt.Errorf("unknown order status %q", name)
	}
	*s = v
	return nil
}

// Order is an API DTO embedding both custom-marshaled types.
type Order struct {
	ID        string      `json:"id"`
	Status    OrderStatus `json:"status"`
	CreatedAt EpochMillis `json:"created_at"`
}
```

### The runnable demo

The demo marshals an `Order` to JSON — showing the integer timestamp and string
status — then unmarshals it back and prints the decoded values.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"time"

	"example.com/apidto"
)

func main() {
	o := apidto.Order{
		ID:        "ord-42",
		Status:    apidto.StatusPaid,
		CreatedAt: apidto.EpochMillis{Time: time.UnixMilli(1719230400000).UTC()},
	}

	b, err := json.Marshal(o)
	if err != nil {
		fmt.Println("marshal error:", err)
		return
	}
	fmt.Println(string(b))

	var back apidto.Order
	if err := json.Unmarshal(b, &back); err != nil {
		fmt.Println("unmarshal error:", err)
		return
	}
	fmt.Printf("decoded status=%s created=%d\n", back.Status, back.CreatedAt.UnixMilli())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"id":"ord-42","status":"paid","created_at":1719230400000}
decoded status=paid created=1719230400000
```

### Tests

`TestRoundTrip` marshals then unmarshals an `Order` and asserts equality of the
fields. `TestWireFormat` asserts the exact bytes: an integer for `created_at` and
a quoted string for `status`. `TestUnknownStatusRejected` decodes an unknown enum
name and asserts an error. `TestEpochMillisPrecision` pins that the millisecond
value survives the round-trip exactly.

Create `apidto_test.go`:

```go
package apidto

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	o := Order{
		ID:        "ord-7",
		Status:    StatusShipped,
		CreatedAt: EpochMillis{Time: time.UnixMilli(1_700_000_000_000).UTC()},
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}

	var back Order
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != o.ID || back.Status != o.Status {
		t.Fatalf("round-trip mismatch: %+v vs %+v", back, o)
	}
	if !back.CreatedAt.Equal(o.CreatedAt.Time) {
		t.Fatalf("time mismatch: %v vs %v", back.CreatedAt.Time, o.CreatedAt.Time)
	}
}

func TestWireFormat(t *testing.T) {
	t.Parallel()

	o := Order{
		ID:        "x",
		Status:    StatusPaid,
		CreatedAt: EpochMillis{Time: time.UnixMilli(1719230400000).UTC()},
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `"status":"paid"`) {
		t.Errorf("status not a quoted string in %q", got)
	}
	if !strings.Contains(got, `"created_at":1719230400000`) {
		t.Errorf("created_at not a bare integer in %q", got)
	}
}

func TestUnknownStatusRejected(t *testing.T) {
	t.Parallel()

	var s OrderStatus
	err := json.Unmarshal([]byte(`"teleported"`), &s)
	if err == nil {
		t.Fatal("decoding an unknown status name must error")
	}
	if !strings.Contains(err.Error(), "teleported") {
		t.Fatalf("error should name the bad value, got %v", err)
	}
}

func TestEpochMillisPrecision(t *testing.T) {
	t.Parallel()

	const ms = 1_234_567_890_123
	var e EpochMillis
	if err := json.Unmarshal([]byte("1234567890123"), &e); err != nil {
		t.Fatal(err)
	}
	if e.UnixMilli() != ms {
		t.Fatalf("UnixMilli = %d, want %d", e.UnixMilli(), ms)
	}
}

func ExampleOrder() {
	o := Order{ID: "a", Status: StatusPending, CreatedAt: EpochMillis{Time: time.UnixMilli(0).UTC()}}
	b, _ := json.Marshal(o)
	fmt.Println(string(b))
	// Output: {"id":"a","status":"pending","created_at":0}
}
```

## Review

The DTO is correct when the wire format is exactly what the API contract promises
— a bare integer for the timestamp, a quoted name for the enum — and when a
round-trip is the identity on the fields that matter. The decode side is where the
value is: `UnmarshalJSON` rejects any status name outside the known set, so
garbage on the wire becomes a clean error rather than a silent zero value. Watch
the receiver rules: `UnmarshalJSON` on a pointer receiver, and decode into an
addressable destination (a struct field, not a map value) or the method never
runs. Run `go test -race`.

## Resources

- [json.Marshaler / json.Unmarshaler](https://pkg.go.dev/encoding/json#Marshaler) — the interfaces `encoding/json` dispatches to.
- [encoding/json](https://pkg.go.dev/encoding/json) — receiver rules, ordering, and the addressability requirement for `Unmarshaler`.
- [encoding/json/v2 proposal](https://go.dev/issue/71497) — `MarshalerTo`/`UnmarshalerFrom`, the modern high-performance direction.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-error-interface-classification.md](05-error-interface-classification.md) | Next: [07-text-marshaler-config.md](07-text-marshaler-config.md)
