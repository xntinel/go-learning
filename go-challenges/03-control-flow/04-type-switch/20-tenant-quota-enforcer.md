# Exercise 20: Apply Tenant Quotas by Decoded Metric Type

**Nivel: Intermedio** — validacion rapida (un test corto).

A multi-tenant API enforces a different quota for each of three usage
dimensions — API calls per billing window, storage bytes, and user seats —
and each dimension is reported to the enforcer as a different decoded
metric type. Given one tenant's configured `Limits` and one incoming
metric, the enforcer must know which limit field that metric's kind maps
to, compare against it, and reject with a specific, actionable error when
the tenant is over.

## What you'll build

```text
tenant-quota-enforcer/       independent module: example.com/tenant-quota-enforcer
  go.mod                     go 1.24
  quotaenforcer.go            Enforce(limits Limits, metric any) error
  cmd/
    demo/
      main.go                enforces limits against three metric kinds
  quotaenforcer_test.go       table test over every metric kind at, under, and over limit
```

- Files: `quotaenforcer.go`, `cmd/demo/main.go`, `quotaenforcer_test.go`.
- Implement: `Enforce(limits Limits, metric any) error`, type-switching on
  `APICallMetric`, `StorageMetric`, and `SeatMetric`.
- Test: each metric kind under, at, and over its limit, plus an unknown
  metric type.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/04-type-switch/20-tenant-quota-enforcer/cmd/demo
cd go-solutions/03-control-flow/04-type-switch/20-tenant-quota-enforcer
go mod edit -go=1.24
```

Each metric kind is a one-field struct rather than a shared
`Metric{Kind string; Value int64}` shape, because that shared shape would
push the "which limit field does this kind compare against" decision into a
string-keyed lookup or a second switch on `Kind` — the type switch already
is that lookup, for free, at compile time, and it is exhaustive over the
concrete types the enforcer accepts even though Go itself does not enforce
exhaustiveness. "At the limit" is deliberately allowed, not rejected: a
tenant provisioned for exactly 1000 API calls should be able to make all
1000, and the boundary condition (`>` rather than `>=`) is one of the
easiest off-by-one mistakes to introduce here, which is why the test table
checks the exact limit value explicitly rather than only checking comfortably
under and over it.

Create `quotaenforcer.go`:

```go
package quotaenforcer

import (
	"errors"
	"fmt"
)

// ErrQuotaExceeded is the sentinel returned when a tenant's usage crosses its
// configured limit, and ErrUnknownMetric is returned for a metric type the
// enforcer has no limit field for.
var (
	ErrQuotaExceeded = errors.New("quota exceeded")
	ErrUnknownMetric = errors.New("unknown metric type")
)

// Limits holds one tenant's configured ceiling for each quota dimension the
// API enforces.
type Limits struct {
	APICalls     int64
	StorageBytes int64
	Seats        int
}

// APICallMetric reports the number of API calls a tenant made in the
// current billing window.
type APICallMetric struct{ Count int64 }

// StorageMetric reports bytes stored by a tenant.
type StorageMetric struct{ Bytes int64 }

// SeatMetric reports active user seats for a tenant.
type SeatMetric struct{ Users int }

// Enforce checks one decoded usage metric against a tenant's limits. Each
// metric kind maps to a different field of Limits and a different unit, so
// the type switch is the lookup: given a metric's concrete type, it knows
// which limit to compare against and how to phrase the violation.
func Enforce(limits Limits, metric any) error {
	switch m := metric.(type) {
	case APICallMetric:
		if m.Count > limits.APICalls {
			return fmt.Errorf("%w: api_calls %d exceeds limit %d", ErrQuotaExceeded, m.Count, limits.APICalls)
		}
		return nil
	case StorageMetric:
		if m.Bytes > limits.StorageBytes {
			return fmt.Errorf("%w: storage_bytes %d exceeds limit %d", ErrQuotaExceeded, m.Bytes, limits.StorageBytes)
		}
		return nil
	case SeatMetric:
		if m.Users > limits.Seats {
			return fmt.Errorf("%w: seats %d exceeds limit %d", ErrQuotaExceeded, m.Users, limits.Seats)
		}
		return nil
	default:
		return fmt.Errorf("%w: %T", ErrUnknownMetric, metric)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tenant-quota-enforcer"
)

func main() {
	limits := quotaenforcer.Limits{APICalls: 1000, StorageBytes: 1 << 20, Seats: 5}
	metrics := []any{
		quotaenforcer.APICallMetric{Count: 1200},
		quotaenforcer.StorageMetric{Bytes: 4096},
		quotaenforcer.SeatMetric{Users: 6},
	}
	for _, m := range metrics {
		if err := quotaenforcer.Enforce(limits, m); err != nil {
			fmt.Printf("rejected: %v\n", err)
			continue
		}
		fmt.Printf("allowed: %T\n", m)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
rejected: quota exceeded: api_calls 1200 exceeds limit 1000
allowed: quotaenforcer.StorageMetric
rejected: quota exceeded: seats 6 exceeds limit 5
```

### Tests

Create `quotaenforcer_test.go`:

```go
package quotaenforcer

import (
	"errors"
	"testing"
)

func TestEnforce(t *testing.T) {
	t.Parallel()
	limits := Limits{APICalls: 1000, StorageBytes: 1 << 20, Seats: 5}

	tests := []struct {
		name    string
		metric  any
		wantErr error
	}{
		{"api calls under limit", APICallMetric{Count: 999}, nil},
		{"api calls at limit is allowed", APICallMetric{Count: 1000}, nil},
		{"api calls over limit", APICallMetric{Count: 1001}, ErrQuotaExceeded},
		{"storage under limit", StorageMetric{Bytes: 1024}, nil},
		{"storage over limit", StorageMetric{Bytes: 1 << 21}, ErrQuotaExceeded},
		{"seats under limit", SeatMetric{Users: 3}, nil},
		{"seats over limit", SeatMetric{Users: 6}, ErrQuotaExceeded},
		{"unknown metric type", "bogus", ErrUnknownMetric},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Enforce(limits, tt.metric)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Enforce(%v) unexpected error: %v", tt.metric, err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Enforce(%v) err = %v, want %v", tt.metric, err, tt.wantErr)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

Enforce is correct because every branch compares strictly-greater-than
against the tenant's limit, so a tenant sitting exactly at their limit is
still allowed — the test table's "at limit is allowed" case is what would
catch a `>=` typo here, which is the single easiest mistake to make in this
kind of code and one that would silently start rejecting tenants who are
paying for exactly the capacity they are using. The `default` branch
returning `ErrUnknownMetric` rather than allowing an unrecognized metric
through by default is the other property worth keeping: a new metric kind
added elsewhere in the codebase without a corresponding `case` here fails
loudly during enforcement instead of being silently un-metered.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [Stripe API: rate limits](https://stripe.com/docs/rate-limits)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-retry-backoff-picker.md](19-retry-backoff-picker.md) | Next: [21-state-machine-event-processor.md](21-state-machine-event-processor.md)
