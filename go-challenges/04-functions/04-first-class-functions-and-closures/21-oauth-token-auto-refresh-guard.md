# Exercise 21: OAuth Token Auto-Refresh Guard with Expiry Window

**Nivel: Intermedio** — validacion rapida (un test corto).

An OAuth client attaches an access token to every outgoing request and must
transparently swap in a new one shortly before the old one expires, instead
of making every call site remember to check expiry. `NewTokenSource` closes
over the current token and memoizes it, refreshing only when the token is
within a configurable window of expiring — the same clock-injection
discipline every time-based module in this lesson uses, so the test drives a
fake clock instead of sleeping real minutes.

## What you'll build

```text
oauth/                     independent module: example.com/oauth-token-auto-refresh
  go.mod                   go 1.24
  oauth.go                 Token, NewTokenSource returns func() Token
  cmd/
    demo/
      main.go               advances a fake clock across the refresh boundary
  oauth_test.go             table test: refresh boundary, two sources isolated
```

- Files: `oauth.go`, `cmd/demo/main.go`, `oauth_test.go`.
- Implement: `NewTokenSource(initial Token, refresh func() Token, now func() time.Time, window time.Duration) func() Token`, closing over the current `token Token`.
- Test: a table advances a fake clock across the 5-minute refresh boundary and checks the token only changes once inside the window and the refresh-call count matches; a second test proves two sources never share captured state.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/oauth/cmd/demo
cd ~/go-exercises/oauth
go mod init example.com/oauth-token-auto-refresh
go mod edit -go=1.24
```

### Memoization with an expiry check instead of a cache key

`NewTokenSource` captures a single `token`, starting at `initial`. Every call
compares `now() + window` against `token.Expiry`: if the token is not
comfortably valid for at least `window` longer, it calls `refresh()` and
replaces the captured token before returning it. This is memoization with a
freshness check baked into the read path, rather than the map-of-keys
memoizer elsewhere in this lesson — there is exactly one thing being cached
(the current token), and the cache invalidates itself based on time rather
than being explicitly cleared.

Taking `now func() time.Time` instead of calling `time.Now()` is what makes
the boundary testable in a single fast test: the table below advances a fake
clock by exact minutes and asserts the token only flips to the refreshed
value once the gap to expiry drops to the window, and that `refresh` is
called exactly once at that point, not on every subsequent call.

This version is deliberately single-goroutine, matching the token-bucket
limiter earlier in this lesson: nothing guards `token`, so calling the
returned closure from multiple goroutines races on it. A real OAuth client
shared across request-handling goroutines needs the closure wrapped in a
`sync.Mutex` — see the TLS-certificate provider elsewhere in this lesson for
the concurrent version of exactly this pattern, where the whole
check-then-refresh runs inside one critical section.

Create `oauth.go`:

```go
package oauth

import "time"

// Token is a minimal OAuth access token: the value a client attaches to
// outgoing requests, and when it stops being valid.
type Token struct {
	Value  string
	Expiry time.Time
}

// NewTokenSource returns a closure that memoizes an OAuth token, refreshing
// it automatically whenever it is within window of expiring. now is injected
// so tests advance a fake clock instead of sleeping; production wires in
// time.Now.
//
// This version is single-goroutine on purpose: nothing guards the captured
// token, so calling the returned closure from multiple goroutines races on
// it. Production wraps the closure in a sync.Mutex, the same way the
// TLS-certificate provider elsewhere in this lesson does for a callback that
// genuinely is called concurrently.
func NewTokenSource(initial Token, refresh func() Token, now func() time.Time, window time.Duration) func() Token {
	token := initial

	return func() Token {
		if !now().Add(window).Before(token.Expiry) {
			token = refresh()
		}
		return token
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/oauth-token-auto-refresh"
)

func main() {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clockNow := start
	clock := func() time.Time { return clockNow }

	refreshCount := 0
	refresh := func() oauth.Token {
		refreshCount++
		return oauth.Token{
			Value:  fmt.Sprintf("token-v%d", refreshCount+1),
			Expiry: clockNow.Add(1 * time.Hour),
		}
	}

	initial := oauth.Token{Value: "token-v1", Expiry: start.Add(10 * time.Minute)}
	getToken := oauth.NewTokenSource(initial, refresh, clock, 5*time.Minute)

	fmt.Println("t+0m:", getToken().Value)

	clockNow = start.Add(3 * time.Minute)
	fmt.Println("t+3m (still outside 5m window):", getToken().Value)

	clockNow = start.Add(6 * time.Minute)
	fmt.Println("t+6m (inside 5m window, refreshed):", getToken().Value)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
t+0m: token-v1
t+3m (still outside 5m window): token-v1
t+6m (inside 5m window, refreshed): token-v2
```

### Tests

Create `oauth_test.go`:

```go
package oauth

import (
	"testing"
	"time"
)

func fakeClock(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	cur := start
	now = func() time.Time { return cur }
	advance = func(d time.Duration) { cur = cur.Add(d) }
	return now, advance
}

func TestGetTokenRefreshesOnlyWithinWindow(t *testing.T) {
	start := time.Unix(0, 0)
	now, advance := fakeClock(start)

	refreshCalls := 0
	refresh := func() Token {
		refreshCalls++
		return Token{Value: "refreshed", Expiry: now().Add(time.Hour)}
	}

	initial := Token{Value: "initial", Expiry: start.Add(10 * time.Minute)}
	getToken := NewTokenSource(initial, refresh, now, 5*time.Minute)

	tests := []struct {
		name        string
		advance     time.Duration
		wantValue   string
		wantRefresh int
	}{
		{"t+0m, far from expiry", 0, "initial", 0},
		{"t+3m, still outside 5m window", 3 * time.Minute, "initial", 0},
		{"t+6m, inside 5m window: refresh", 3 * time.Minute, "refreshed", 1},
		{"t+7m, freshly refreshed token still valid", time.Minute, "refreshed", 1},
	}

	for _, tc := range tests {
		advance(tc.advance)
		got := getToken()
		if got.Value != tc.wantValue {
			t.Fatalf("%s: token = %q, want %q", tc.name, got.Value, tc.wantValue)
		}
		if refreshCalls != tc.wantRefresh {
			t.Fatalf("%s: refreshCalls = %d, want %d", tc.name, refreshCalls, tc.wantRefresh)
		}
	}
}

func TestTwoTokenSourcesDoNotShareToken(t *testing.T) {
	now, _ := fakeClock(time.Unix(0, 0))
	refreshA := func() Token { return Token{Value: "a-refreshed", Expiry: now().Add(time.Hour)} }
	refreshB := func() Token { return Token{Value: "b-refreshed", Expiry: now().Add(time.Hour)} }

	a := NewTokenSource(Token{Value: "a-initial", Expiry: now().Add(time.Hour)}, refreshA, now, time.Minute)
	b := NewTokenSource(Token{Value: "b-initial", Expiry: now().Add(time.Hour)}, refreshB, now, time.Minute)

	if got := a().Value; got != "a-initial" {
		t.Fatalf("a() = %q, want a-initial", got)
	}
	if got := b().Value; got != "b-initial" {
		t.Fatalf("b() = %q, want b-initial — sources must not share captured token state", got)
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The table walks the exact 5-minute boundary: far from expiry, still outside
the window, then just inside it — where the token flips and `refreshCalls`
becomes 1 — then one more minute later where the freshly refreshed token is
still valid and `refresh` is not called a second time. The isolation test
confirms the structural guarantee this whole lesson relies on: two calls to
`NewTokenSource` allocate two separate captured `token` variables, so
draining or refreshing one never touches the other.

## Resources

- [pkg.go.dev: golang.org/x/oauth2](https://pkg.go.dev/golang.org/x/oauth2#TokenSource) — the real `TokenSource` interface this closure's shape mirrors.
- [pkg.go.dev: time.Time.Before](https://pkg.go.dev/time#Time.Before) — the comparison used to decide whether the token is still within its window.
- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — how the returned closure captures `token`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-multi-layer-cache-fallback-chain.md](20-multi-layer-cache-fallback-chain.md) | Next: [22-audit-log-factory-with-tenant-context.md](22-audit-log-factory-with-tenant-context.md)
