# Exercise 28: Dispatch DNS Resolved Records by Type to Handlers

**Nivel: Intermedio** — validacion rapida (un test corto).

A service discovery layer that resolves DNS itself — rather than trusting
a single upstream A record — has to deal with four structurally different
record shapes coming back from the same lookup: an `A` record giving an
IPv4 address, an `AAAA` record giving IPv6, an `SRV` record giving a
service's target host and port with priority and weight for load
balancing, and a `TXT` record carrying arbitrary text used for domain
ownership verification. Each shape validates differently, expires
differently against its own TTL, and needs its own handler downstream. A
record dispatcher that does not classify these correctly before validating
will either crash on a record it read the wrong field from, or silently
accept a malformed one. This module is fully self-contained: its own `go
mod init`, all code inline, its own demo and tests.

## What you'll build

```text
dns-resolution-record-dispatch/   independent module: example.com/dns-resolution-record-dispatch
  go.mod                          go 1.24
  dnsdispatch.go                  Dispatch(rec any, fetchedAt, now time.Time) (Resolved, error)
  cmd/
    demo/
      main.go                     dispatches an A, AAAA, SRV, and expired TXT record
  dnsdispatch_test.go               table of valid, malformed, and expired cases per record kind
```

- Files: `dnsdispatch.go`, `cmd/demo/main.go`, `dnsdispatch_test.go`.
- Implement: `Dispatch(rec any, fetchedAt, now time.Time) (Resolved,
  error)`, type-switching on `ARecord`, `AAAARecord`, `SRVRecord`, and
  `TXTRecord` to validate each kind's required fields before checking TTL
  expiry.
- Test: a valid case per record kind, a missing-field case per record kind,
  an address swapped into the wrong IP-version field, an expired TTL, and
  an unsupported record type.

Set up the module:

```bash
go mod edit -go=1.24
```

The TTL check runs identically for every record kind — compute
`fetchedAt + TTL` and compare against `now` — so it is deliberately
factored to run once, after the type switch, rather than duplicated inside
each `case`. What genuinely differs per kind is which fields are required
and how they are validated: an `ARecord` needs `net.ParseIP` to succeed
*and* the parsed address to have a non-nil `To4()`, because `net.ParseIP`
alone accepts an IPv6 literal without complaint, and a resolver that only
checks "did this parse as *some* IP" will silently hand an `AAAA`-shaped
address to a caller expecting IPv4. `AAAARecord` inverts that same check
(`To4() != nil` rejects an IPv4-mapped or plain IPv4 literal from ending up
classified as IPv6). `fetchedAt` and `now` are both explicit parameters
rather than one call to `time.Now()` inside the function, so that
`Dispatch` is a pure function of its inputs — replaying a captured
resolution trace during an incident review reproduces the exact same
expiry decision every time, independent of when the replay actually runs.

Create `dnsdispatch.go`:

```go
package dnsdispatch

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrMalformedRecord is returned for a record missing a required field or
// carrying a value that fails basic syntactic validation.
var ErrMalformedRecord = errors.New("dnsdispatch: malformed record")

// ErrExpired is returned for a record whose TTL has already elapsed by the
// time it reaches Dispatch.
var ErrExpired = errors.New("dnsdispatch: record expired")

// ARecord maps a name to an IPv4 address.
type ARecord struct {
	Name string
	IPv4 string
	TTL  int // seconds
}

// AAAARecord maps a name to an IPv6 address.
type AAAARecord struct {
	Name string
	IPv6 string
	TTL  int
}

// SRVRecord advertises a service's target host and port, with a priority
// and weight for load balancing across multiple SRV records for the same
// service.
type SRVRecord struct {
	Service  string
	Target   string
	Port     uint16
	Priority uint16
	Weight   uint16
	TTL      int
}

// TXTRecord carries arbitrary text, commonly used for domain ownership
// verification (SPF, DKIM, ACME challenges).
type TXTRecord struct {
	Name string
	Text string
	TTL  int
}

// Resolved is the normalized shape every record kind is dispatched into,
// regardless of which of the four wire formats it started as.
type Resolved struct {
	Kind      string
	Value     string
	ExpiresAt time.Time
}

// Dispatch validates rec's required fields for its concrete type and
// computes its expiry from fetchedAt+TTL, rejecting it if that expiry is
// already behind now. fetchedAt and now are passed explicitly rather than
// read from time.Now(), so resolution is a pure, reproducible function of
// its inputs — the same record dispatched against the same two timestamps
// always produces the same result, which matters for replaying a captured
// resolution trace during an incident review.
func Dispatch(rec any, fetchedAt, now time.Time) (Resolved, error) {
	var kind, value string
	var ttl int

	switch r := rec.(type) {
	case ARecord:
		if r.Name == "" || r.IPv4 == "" {
			return Resolved{}, fmt.Errorf("%w: A record missing name or address", ErrMalformedRecord)
		}
		ip := net.ParseIP(r.IPv4)
		if ip == nil || ip.To4() == nil {
			return Resolved{}, fmt.Errorf("%w: %q is not a valid IPv4 address", ErrMalformedRecord, r.IPv4)
		}
		kind, value, ttl = "A", r.IPv4, r.TTL

	case AAAARecord:
		if r.Name == "" || r.IPv6 == "" {
			return Resolved{}, fmt.Errorf("%w: AAAA record missing name or address", ErrMalformedRecord)
		}
		ip := net.ParseIP(r.IPv6)
		if ip == nil || ip.To4() != nil {
			return Resolved{}, fmt.Errorf("%w: %q is not a valid IPv6 address", ErrMalformedRecord, r.IPv6)
		}
		kind, value, ttl = "AAAA", r.IPv6, r.TTL

	case SRVRecord:
		if r.Service == "" || r.Target == "" || r.Port == 0 {
			return Resolved{}, fmt.Errorf("%w: SRV record missing service, target, or port", ErrMalformedRecord)
		}
		kind, value, ttl = "SRV", fmt.Sprintf("%s:%d", r.Target, r.Port), r.TTL

	case TXTRecord:
		if r.Name == "" {
			return Resolved{}, fmt.Errorf("%w: TXT record missing name", ErrMalformedRecord)
		}
		kind, value, ttl = "TXT", r.Text, r.TTL

	default:
		return Resolved{}, fmt.Errorf("%w: unsupported record type %T", ErrMalformedRecord, rec)
	}

	expiresAt := fetchedAt.Add(time.Duration(ttl) * time.Second)
	if now.After(expiresAt) {
		return Resolved{}, fmt.Errorf("%w: %s record expired at %s", ErrExpired, kind, expiresAt.Format(time.RFC3339))
	}
	return Resolved{Kind: kind, Value: value, ExpiresAt: expiresAt}, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/dns-resolution-record-dispatch"
)

func main() {
	fetchedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := fetchedAt.Add(90 * time.Second)

	records := []any{
		dnsdispatch.ARecord{Name: "api.example.com", IPv4: "203.0.113.10", TTL: 300},
		dnsdispatch.AAAARecord{Name: "api.example.com", IPv6: "2001:db8::1", TTL: 300},
		dnsdispatch.SRVRecord{Service: "_sip._tcp", Target: "sip.example.com", Port: 5060, TTL: 300},
		dnsdispatch.TXTRecord{Name: "example.com", Text: "v=spf1 -all", TTL: 60},
	}

	for _, rec := range records {
		resolved, err := dnsdispatch.Dispatch(rec, fetchedAt, now)
		if err != nil {
			fmt.Printf("%-22T -> error: %v\n", rec, err)
			continue
		}
		fmt.Printf("%-22T -> %s %s\n", rec, resolved.Kind, resolved.Value)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
dnsdispatch.ARecord    -> A 203.0.113.10
dnsdispatch.AAAARecord -> AAAA 2001:db8::1
dnsdispatch.SRVRecord  -> SRV sip.example.com:5060
dnsdispatch.TXTRecord  -> error: dnsdispatch: record expired: TXT record expired at 2026-06-01T12:01:00Z
```

The `TXT` record's 60-second TTL has already elapsed by the time `now` is
90 seconds past `fetchedAt`, while the other three records' 300-second TTL
still has room. Each record kind's expiry is computed and checked
independently, using the same `fetchedAt`/`now` pair for all four.

### Tests

Create `dnsdispatch_test.go`:

```go
package dnsdispatch

import (
	"errors"
	"testing"
	"time"
)

func TestDispatch(t *testing.T) {
	t.Parallel()
	fetchedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := fetchedAt.Add(30 * time.Second)

	tests := []struct {
		name    string
		rec     any
		wantErr error
	}{
		{
			name: "valid A record resolves",
			rec:  ARecord{Name: "api.example.com", IPv4: "203.0.113.10", TTL: 300},
		},
		{
			name:    "A record missing address is malformed",
			rec:     ARecord{Name: "api.example.com", TTL: 300},
			wantErr: ErrMalformedRecord,
		},
		{
			name:    "A record with an IPv6 address in the IPv4 field is malformed",
			rec:     ARecord{Name: "api.example.com", IPv4: "2001:db8::1", TTL: 300},
			wantErr: ErrMalformedRecord,
		},
		{
			name: "valid AAAA record resolves",
			rec:  AAAARecord{Name: "api.example.com", IPv6: "2001:db8::1", TTL: 300},
		},
		{
			name:    "AAAA record with an IPv4 address in the IPv6 field is malformed",
			rec:     AAAARecord{Name: "api.example.com", IPv6: "203.0.113.10", TTL: 300},
			wantErr: ErrMalformedRecord,
		},
		{
			name: "valid SRV record resolves",
			rec:  SRVRecord{Service: "_sip._tcp", Target: "sip.example.com", Port: 5060, TTL: 300},
		},
		{
			name:    "SRV record missing port is malformed",
			rec:     SRVRecord{Service: "_sip._tcp", Target: "sip.example.com", TTL: 300},
			wantErr: ErrMalformedRecord,
		},
		{
			name: "valid TXT record resolves",
			rec:  TXTRecord{Name: "example.com", Text: "v=spf1 -all", TTL: 300},
		},
		{
			name:    "TXT record missing name is malformed",
			rec:     TXTRecord{Text: "v=spf1 -all", TTL: 300},
			wantErr: ErrMalformedRecord,
		},
		{
			name:    "record past its TTL is expired",
			rec:     ARecord{Name: "api.example.com", IPv4: "203.0.113.10", TTL: 10},
			wantErr: ErrExpired,
		},
		{
			name:    "unsupported record type is malformed",
			rec:     "not-a-record",
			wantErr: ErrMalformedRecord,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Dispatch(tt.rec, fetchedAt, now)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Dispatch(%v) unexpected error: %v", tt.rec, err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Dispatch(%v) err = %v, want %v", tt.rec, err, tt.wantErr)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Dispatch` is correct because the IPv4/IPv6 validation for `ARecord` and
`AAAARecord` checks both that the string parses as an IP address *and*
that it parses as the right address family — `net.ParseIP` alone happily
accepts either family for either field, so skipping the `To4()` check
would let an `AAAARecord` silently carry an IPv4 address all the way to a
caller that assumes every value it gets back is genuinely IPv6. Computing
`expiresAt` and comparing it against `now` once, after the type switch,
rather than inside each `case`, is what keeps the four record kinds from
ever drifting into four slightly different expiry rules — a duplicated
comparison is exactly the kind of thing that gets fixed in three places and
missed in the fourth. The `default` case returning `ErrMalformedRecord`
rather than silently ignoring an unrecognized record type is what makes a
newly introduced record kind — a `CNAME` or `MX` record added to the
resolver elsewhere without a corresponding `case` here — fail loudly during
dispatch instead of vanishing.

## Resources

- [RFC 1035: Domain Names — Implementation and Specification](https://www.rfc-editor.org/rfc/rfc1035)
- [RFC 2782: A DNS RR for specifying the location of services (SRV)](https://www.rfc-editor.org/rfc/rfc2782)
- [net.ParseIP](https://pkg.go.dev/net#ParseIP)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-admission-control-load-shedding.md](27-admission-control-load-shedding.md) | Next: [29-connection-pool-route-selection.md](29-connection-pool-route-selection.md)
