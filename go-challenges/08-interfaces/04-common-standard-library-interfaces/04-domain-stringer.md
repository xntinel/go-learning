# Exercise 4: fmt.Stringer for a Domain Enum and a Redacted Credential Type

`fmt.Stringer` is the interface `fmt` calls automatically for `%v` and `%s`. Two
real uses: give a domain enum a stable human-readable name so logs and errors read
cleanly, and give a credential type a `String()` that masks itself so it can never
leak in a log line or a `%v` dump. Both are one method — and both have a classic
infinite-recursion trap.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
domainstr/                  independent module: example.com/domainstr
  go.mod
  domainstr.go              OrderStatus enum with String(); APIKey with masking String()
  cmd/
    demo/
      main.go               prints a status and a key via %s and %v
  domainstr_test.go         enum names, unknown fallback, masking, no-recursion guard
```

- Files: `domainstr.go`, `cmd/demo/main.go`, `domainstr_test.go`.
- Implement: `String() string` on an `OrderStatus` enum (stable names, defined fallback for unknown values) and on an `APIKey` string type (masks all but the last four characters).
- Test: each enum value maps to its expected string and an unknown value maps to the fallback; `APIKey.String()` masks the secret and `fmt.Sprintf("%v", key)` uses it; a guard proving `String()` does not recurse.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/domainstr/cmd/demo
cd ~/go-exercises/domainstr
go mod init example.com/domainstr
```

### How fmt dispatches, and the recursion trap

When `fmt` formats a value with `%v` or `%s`, it checks whether the value
implements `fmt.Stringer` and, if so, calls `String()` and prints the result. That
is why `fmt.Println(status)` prints `shipped` instead of `3`. The trap is subtle:
if you implement `String()` on a value type and its body is
`fmt.Sprintf("%v", self)`, then `fmt` sees a `Stringer`, calls `String()`, which
calls `fmt`, which calls `String()` — unbounded recursion until the stack
overflows. The fix is to convert the receiver to its *underlying* type first, so
`fmt` no longer sees a `Stringer`: for the enum, format `int(s)`; for the key,
slice `string(k)`. Neither re-enters the method.

For `OrderStatus`, the `String()` uses a `switch` returning fixed names and a
`default` that returns a defined fallback (`"unknown"`) for any value not in the
set — never a panic and never an empty string, so a corrupt or newly-added status
still prints something legible. For `APIKey`, `String()` shows only the last four
characters behind a mask, so even if someone logs the key with `%v` (the mistake
this defends against), the secret body never appears. The key still holds its full
value for real use (`string(k)`); only its *printed* form is masked.

Create `domainstr.go`:

```go
package domainstr

// OrderStatus is the lifecycle state of an order. It implements fmt.Stringer so
// logs and errors show a stable name rather than an integer.
type OrderStatus int

const (
	StatusUnknown OrderStatus = iota
	StatusPending
	StatusPaid
	StatusShipped
	StatusDelivered
	StatusCancelled
)

func (s OrderStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusPaid:
		return "paid"
	case StatusShipped:
		return "shipped"
	case StatusDelivered:
		return "delivered"
	case StatusCancelled:
		return "cancelled"
	default:
		// Defined fallback for StatusUnknown and any out-of-range value.
		// Note: int(s), not %v of the receiver, so this cannot recurse.
		return "unknown"
	}
}

// APIKey is a secret credential. Its String() masks all but the last four
// characters so the secret never leaks through %v or %s formatting.
type APIKey string

func (k APIKey) String() string {
	s := string(k) // convert to the underlying type: no recursion via fmt
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

// Reveal returns the full secret for legitimate use (e.g. an Authorization
// header). Only the String form is masked.
func (k APIKey) Reveal() string { return string(k) }
```

### The runnable demo

The demo prints a status through `%s` and a key through `%v`, showing that `fmt`
dispatches to `String()` for both verbs, and that the key is masked while its real
value remains available via `Reveal`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/domainstr"
)

func main() {
	status := domainstr.StatusShipped
	key := domainstr.APIKey("sk-live-1234567890abcd")

	fmt.Printf("status=%s key=%v\n", status, key)
	fmt.Printf("unknown status prints as: %s\n", domainstr.OrderStatus(99))
	fmt.Printf("real key length=%d\n", len(key.Reveal()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=shipped key=****abcd
unknown status prints as: unknown
real key length=22
```

### Tests

`TestStatusNames` is table-driven over every enum value plus an out-of-range one,
asserting the fallback. `TestAPIKeyMasks` asserts the masked form and that the
secret body does not appear. `TestFmtUsesStringer` proves `fmt.Sprintf("%v", key)`
routes through `String()`. `TestStringDoesNotRecurse` is the guard: it formats
values that, with a `Sprintf("%v", self)` implementation, would overflow the
stack; reaching the assertion at all proves `String()` converts to the underlying
type instead.

Create `domainstr_test.go`:

```go
package domainstr

import (
	"fmt"
	"strings"
	"testing"
)

func TestStatusNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status OrderStatus
		want   string
	}{
		{StatusPending, "pending"},
		{StatusPaid, "paid"},
		{StatusShipped, "shipped"},
		{StatusDelivered, "delivered"},
		{StatusCancelled, "cancelled"},
		{StatusUnknown, "unknown"},
		{OrderStatus(99), "unknown"}, // out-of-range falls back
	}
	for _, tc := range tests {
		if got := tc.status.String(); got != tc.want {
			t.Errorf("OrderStatus(%d).String() = %q, want %q", int(tc.status), got, tc.want)
		}
	}
}

func TestAPIKeyMasks(t *testing.T) {
	t.Parallel()

	key := APIKey("sk-live-1234567890abcd")
	got := key.String()
	if got != "****abcd" {
		t.Fatalf("String() = %q, want %q", got, "****abcd")
	}
	if strings.Contains(got, "1234567890") {
		t.Fatalf("masked form %q leaks the secret body", got)
	}
	if key.Reveal() != "sk-live-1234567890abcd" {
		t.Fatal("Reveal must return the full secret")
	}

	short := APIKey("ab")
	if short.String() != "****" {
		t.Fatalf("short key String() = %q, want %q", short.String(), "****")
	}
}

func TestFmtUsesStringer(t *testing.T) {
	t.Parallel()

	key := APIKey("token-wxyz")
	if got := fmt.Sprintf("%v", key); got != "****wxyz" {
		t.Fatalf("%%v = %q, want %q", got, "****wxyz")
	}
	if got := fmt.Sprintf("%s", StatusPaid); got != "paid" {
		t.Fatalf("%%s = %q, want %q", got, "paid")
	}
}

func TestStringDoesNotRecurse(t *testing.T) {
	t.Parallel()

	// If String() were implemented as fmt.Sprintf("%v", self), formatting
	// here would recurse until the stack overflows. Reaching the assertions
	// proves the implementations convert to the underlying type.
	_ = fmt.Sprintf("%v %s", APIKey("abcdefgh"), StatusShipped)
}

func ExampleOrderStatus_String() {
	fmt.Println(StatusDelivered)
	// Output: delivered
}
```

## Review

The types are correct when every enum value prints its stable name, an unknown
value prints the defined fallback, and the API key shows only its last four
characters through both `%v` and `%s`. The mistake this module is built around is
`fmt.Sprintf("%v", self)` inside `String()`, which recurses forever; both
implementations convert to the underlying type (`int(s)`, `string(k)`) to avoid
it, and `TestStringDoesNotRecurse` would blow the stack if they did not. A masking
`String()` is a real defence-in-depth measure: it means an accidental
`log.Printf("%v", key)` cannot spill the credential. Run `go test -race`.

## Resources

- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer) — the interface `fmt` dispatches for `%v` and `%s`.
- [fmt package](https://pkg.go.dev/fmt) — how formatting verbs consult `Stringer`, `Formatter`, and `GoStringer`.
- [Go stringer tool](https://pkg.go.dev/golang.org/x/tools/cmd/stringer) — code-generates `String()` for enum types.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-redacting-writer.md](03-redacting-writer.md) | Next: [05-error-interface-classification.md](05-error-interface-classification.md)
