# Exercise 19: DNS Lookup Cache: Fresh Lookup or Stale Fallback

**Nivel: Intermedio** — validacion rapida (un test corto).

A DNS resolver timing out for thirty seconds during a network blip should
not take your service down with it if you already know a recent, probably
still-correct answer. Falling back to a stale cache entry when a fresh
lookup fails is a deliberate availability-over-consistency trade, and it
only holds up if the fallback itself is bounded — a cache entry from three
days ago is not a safe substitute for a timed-out lookup. This module is
fully self-contained: its own `go mod init`, all code inline, its own test
file.

## What you'll build

```text
dnscache/                   independent module: example.com/dns-lookup-cache-stale-fallback
  go.mod                    go 1.24
  resolve.go                Resolve(host, now, staleTTL, cache, fresh) (ip, stale, err)
  resolve_test.go           table: fresh ok, fresh fails+cache hit, fresh fails+too stale, fresh fails+no entry
```

- Files: `resolve.go`, `resolve_test.go`.
- Implement: `Resolve(host string, now time.Time, staleTTL time.Duration, cache map[string]Entry, fresh func(string) (string, error)) (ip string, stale bool, err error)`, nesting a comma-ok cache read inside the fresh-lookup failure branch.
- Test: a table over a fresh lookup succeeding outright, a fresh failure falling back to a usable cache entry, a fresh failure with an entry too old to trust, and a fresh failure with no cache entry at all.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/01-if-else-and-init-statements/19-dns-lookup-cache-stale-fallback
cd go-solutions/03-control-flow/01-if-else-and-init-statements/19-dns-lookup-cache-stale-fallback
go mod edit -go=1.24
```

### Why the cache read is nested inside the failure branch, not run first

Checking the cache before attempting a fresh lookup would make the cache
authoritative and the network lookup a mere refresh — the opposite of what
this resolver should do. A fresh, correct answer is always preferable to a
cached one; the cache exists purely to survive a fresh lookup's failure.
Nesting `cache[host]` inside the `if _, err := fresh(host); err != nil`
branch encodes that priority directly in the control flow: the cache is
never even consulted on the common path where the network answers
normally, and it is consulted with a second guard — the staleness check —
before being trusted as a substitute.

Create `resolve.go`:

```go
// Package dnscache resolves a hostname via a fresh lookup, falling back to a
// cached result only when the fresh lookup fails and the cached entry is not
// too old to trust.
package dnscache

import (
	"errors"
	"time"
)

// Entry is one cached resolution result.
type Entry struct {
	IP         string
	ResolvedAt time.Time
}

// ErrNoResolution means the fresh lookup failed and no usable cached entry
// existed to fall back on.
var ErrNoResolution = errors.New("dns resolution failed and no usable stale cache entry")

// Resolve resolves host via fresh at now. If fresh fails, Resolve falls back
// to cache[host] provided that entry is no older than staleTTL; the returned
// stale flag tells the caller whether the answer came from the fallback path
// so it can log or monitor stale-serve rates.
func Resolve(host string, now time.Time, staleTTL time.Duration, cache map[string]Entry, fresh func(string) (string, error)) (ip string, stale bool, err error) {
	if ip, err := fresh(host); err == nil {
		return ip, false, nil
	}

	entry, ok := cache[host]
	if !ok {
		return "", false, ErrNoResolution
	}

	if age := now.Sub(entry.ResolvedAt); age > staleTTL {
		return "", false, ErrNoResolution
	}

	return entry.IP, true, nil
}
```

### Tests

The table drives `fresh` with a stub function per case so each branch —
success, failure-with-fresh-cache, failure-with-stale-cache,
failure-with-no-entry — is reached without a real network dependency.

Create `resolve_test.go`:

```go
package dnscache

import (
	"errors"
	"testing"
	"time"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	staleTTL := 10 * time.Minute
	failing := func(string) (string, error) { return "", errors.New("timeout") }
	succeeding := func(string) (string, error) { return "203.0.113.5", nil }

	tests := []struct {
		name      string
		cache     map[string]Entry
		fresh     func(string) (string, error)
		wantIP    string
		wantStale bool
		wantErr   error
	}{
		{
			name:      "fresh lookup succeeds, cache is irrelevant",
			cache:     map[string]Entry{"example.com": {IP: "198.51.100.1", ResolvedAt: now.Add(-1 * time.Minute)}},
			fresh:     succeeding,
			wantIP:    "203.0.113.5",
			wantStale: false,
		},
		{
			name:      "fresh fails, falls back to a fresh-enough cache entry",
			cache:     map[string]Entry{"example.com": {IP: "198.51.100.1", ResolvedAt: now.Add(-5 * time.Minute)}},
			fresh:     failing,
			wantIP:    "198.51.100.1",
			wantStale: true,
		},
		{
			name:    "fresh fails, cache entry is too old to trust",
			cache:   map[string]Entry{"example.com": {IP: "198.51.100.1", ResolvedAt: now.Add(-time.Hour)}},
			fresh:   failing,
			wantErr: ErrNoResolution,
		},
		{
			name:    "fresh fails, no cache entry at all",
			cache:   map[string]Entry{},
			fresh:   failing,
			wantErr: ErrNoResolution,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ip, stale, err := Resolve("example.com", now, staleTTL, tc.cache, tc.fresh)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if ip != tc.wantIP {
				t.Errorf("ip = %q, want %q", ip, tc.wantIP)
			}
			if stale != tc.wantStale {
				t.Errorf("stale = %v, want %v", stale, tc.wantStale)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The `stale` return value is what makes this fallback observable in
production: a resolver that silently serves a three-day-old IP behind an
identical-looking success response hides an availability trade-off an
operator would want to know is happening. Carry this forward: whenever a
guard chain's failure path substitutes an approximate answer for an exact
one, surface that substitution to the caller instead of making the two
paths indistinguishable.

## Resources

- [RFC 8767: Serving Stale Data to Improve DNS Resiliency](https://www.rfc-editor.org/rfc/rfc8767) — the standard this exercise's fallback strategy mirrors.
- [Go Specification: Index expressions](https://go.dev/ref/spec#Index_expressions) — the comma-ok map read used for the cache lookup.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-message-schema-version-negotiation.md](18-message-schema-version-negotiation.md) | Next: [20-blob-storage-retry-exponential-backoff.md](20-blob-storage-retry-exponential-backoff.md)
