# Exercise 3: API Response DTO — Deterministic Output from json.Marshal

The single most valuable thing an example can pin on a backend is the exact wire
format an endpoint returns. Here you build an order-status response DTO and an
example that marshals it and pins the resulting bytes, turning the JSON contract
into an executed regression guard — and you learn precisely which stdout is
deterministic enough to pin.

## What you'll build

```text
statusdto/                  independent module: example.com/statusdto
  go.mod                    go 1.26
  status.go                 type Status; MarshalStatus(Status) ([]byte, error)
  cmd/
    demo/
      main.go               runnable demo printing the JSON payload
  status_test.go            table-driven Test + ExampleMarshalStatus, ExampleMarshalStatus_map
```

Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
Implement: a `Status` struct with JSON tags and a `MarshalStatus` helper wrapping `json.Marshal`.
Test: a table-driven `Test`, plus `ExampleMarshalStatus` pinning the exact JSON bytes and `ExampleMarshalStatus_map` showing map-key sorting.
Verify: `go test -count=1 -race ./...`

## Which stdout is safe to pin

An executed example bets that its stdout is reproducible. `json.Marshal` gives you
two forms of determinism that make it perfect for pinning wire contracts. First,
a struct marshals its fields in declaration order, so the byte sequence is fixed
by the source. Second — and this is the part worth internalizing —
`json.Marshal` of a `map[string]T` sorts the keys, so even map-derived JSON is
stable and safe to pin. Contrast that with `fmt.Println` of the same raw map,
which prints in Go's randomized iteration order and would flake. So the rule is
sharp: JSON from `Marshal` is pinnable; a raw map through `fmt` is not.

Because the bytes are fixed by the struct shape, the example doubles as a
wire-contract regression guard: add a field to `Status`, or rename a JSON tag,
and `ExampleMarshalStatus` fails until you update the `// Output:` line — which is
exactly the loud, legible signal you want a consumer to see when the payload
changes shape. `ExampleMarshalStatus_map` demonstrates the map-sorting property
directly, so a reader sees why map-derived JSON is deterministic while `fmt` of a
map is not.

Create `status.go`:

```go
package statusdto

import "encoding/json"

// Status is the JSON body an order-status endpoint returns. Field order in the
// struct fixes field order in the marshaled bytes, so its wire form is stable.
type Status struct {
	OrderID string `json:"order_id"`
	State   string `json:"state"`
	Paid    bool   `json:"paid"`
}

// MarshalStatus returns the JSON encoding of s. Naming the example after this
// function (ExampleMarshalStatus) attaches it to the symbol in go doc.
func MarshalStatus(s Status) ([]byte, error) {
	return json.Marshal(s)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/statusdto"
)

func main() {
	s := statusdto.Status{OrderID: "A-100", State: "shipped", Paid: true}
	b, err := statusdto.MarshalStatus(s)
	if err != nil {
		fmt.Println("marshal error:", err)
		return
	}
	fmt.Println(string(b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"order_id":"A-100","state":"shipped","paid":true}
```

### Tests and examples

The table-driven `Test` marshals a few payloads and asserts the exact bytes; the
examples pin the contract as documentation. `ExampleMarshalStatus_map` marshals a
`map[string]int` to show that `json.Marshal` sorts keys, so its output is
deterministic.

Create `status_test.go`:

```go
package statusdto

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestMarshal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Status
		want string
	}{
		{"paid shipped", Status{"A-100", "shipped", true}, `{"order_id":"A-100","state":"shipped","paid":true}`},
		{"unpaid pending", Status{"B-200", "pending", false}, `{"order_id":"B-200","state":"pending","paid":false}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, err := MarshalStatus(tt.in)
			if err != nil {
				t.Fatalf("MarshalStatus: %v", err)
			}
			if got := string(b); got != tt.want {
				t.Errorf("MarshalStatus = %s, want %s", got, tt.want)
			}
		})
	}
}

func ExampleMarshalStatus() {
	s := Status{OrderID: "A-100", State: "shipped", Paid: true}
	b, _ := MarshalStatus(s)
	fmt.Println(string(b))
	// Output: {"order_id":"A-100","state":"shipped","paid":true}
}

func ExampleMarshalStatus_map() {
	// json.Marshal sorts map keys, so this output is deterministic and pinnable.
	b, _ := json.Marshal(map[string]int{"c": 3, "a": 1, "b": 2})
	fmt.Println(string(b))
	// Output: {"a":1,"b":2,"c":3}
}
```

## Review

The example is correct when the pinned bytes are the endpoint's real wire form,
so treat a failing `ExampleMarshalStatus` as a wire-contract change, not a test
nuisance: add a `Refunded bool` field to `Status` and the example fails until the
`// Output:` line reflects the new payload, which is the point — it forces the
doc and the contract to move together. The determinism lesson is the one to carry
out: `json.Marshal` sorts map keys (`ExampleMarshalStatus_map` proves it), so
map-derived JSON is safe to pin, while `fmt.Println` of a raw map is not and must
never appear under a plain `// Output:`. Keep `gofmt -l` empty and `go vet ./...`
clean.

## Resources

- [encoding/json.Marshal](https://pkg.go.dev/encoding/json#Marshal) — field ordering for structs and the documented map-key sorting.
- [testing package — Examples](https://pkg.go.dev/testing#hdr-Examples) — the `// Output:` comparison rules.
- [The Go Blog: JSON and Go](https://go.dev/blog/json) — how Go encodes structs and maps to JSON.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-money-value-object-method-examples.md](02-money-value-object-method-examples.md) | Next: [04-unordered-output-header-set.md](04-unordered-output-header-set.md)
