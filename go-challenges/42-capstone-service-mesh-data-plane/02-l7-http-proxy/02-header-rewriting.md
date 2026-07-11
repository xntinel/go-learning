# Exercise 2: Header Rewriting

Forwarding a request correctly is mostly a header problem: an L7 proxy must remove the headers that describe a single connection hop, honor the sender's dynamic nominations, and apply the operator's own add/set/remove policy. This exercise builds that rewriting layer as a standalone module — a two-pass hop-by-hop strip and an ordered rule applier over `http.Header`.

This module is fully self-contained: its own `go mod init`, every type it needs defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
headers.go           HeaderRule, StripHopByHop (two-pass), ApplyHeaderRules
cmd/
  demo/
    main.go          strip hop-by-hop from a header set, then apply rules
headers_test.go      standard set, Connection nomination, multi-line Connection, rule actions
```

- Files: `headers.go`, `cmd/demo/main.go`, `headers_test.go`.
- Implement: `StripHopByHop(h http.Header)` and `ApplyHeaderRules(h http.Header, rules []HeaderRule)`, plus the `HeaderRule` value type.
- Test: the standard hop-by-hop set is removed, a `Connection`-nominated header is removed, multiple `Connection` lines each take effect, and add/set/remove behave and respect order.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p header-rewriting/cmd/demo && cd header-rewriting
go mod init example.com/header-rewriting
go mod edit -go=1.26
```

### The two-pass strip and the raw Connection slice

Hop-by-hop headers describe one connection hop and must never be forwarded. There are two kinds. The static set is fixed by RFC 9110 section 7.6.1: `Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailers`, `Transfer-Encoding`, `Upgrade`. The dynamic set is whatever the sender nominates through the `Connection` header: `Connection: close, X-Internal` declares `X-Internal` hop-by-hop for this one hop. A correct strip is therefore two passes, and the dynamic pass must run *first* — it reads the `Connection` header values, and the static pass then deletes `Connection` itself, so reversing the order would discard the nominations before they were read.

The dynamic pass has a subtlety in how it reads the header. The code uses `h["Connection"]` — direct map indexing — rather than `h.Get("Connection")`. `Get` canonicalizes the name and returns only the first value; indexing returns the raw slice of every `Connection` line. A request can legitimately carry more than one `Connection` line, each nominating different headers, so enumerating the slice and splitting each value on commas catches every nomination. Each nominated name is trimmed of surrounding whitespace and, if non-empty, deleted.

`ApplyHeaderRules` is the operator-policy layer that runs after stripping. Each `HeaderRule` is an action — `add` appends a value (keeping any existing ones), `set` replaces all values, `remove` deletes the header — applied in slice order. Order matters: a `set` followed by a `remove` of the same header leaves it absent, and the reverse leaves it present, so the rule slice is read top to bottom exactly as written.

Create `headers.go`:

```go
package headers

import (
	"net/http"
	"strings"
)

// HeaderRule adds, sets, or removes one header on a request.
type HeaderRule struct {
	Action string // "add", "set", or "remove"
	Name   string
	Value  string // ignored for "remove"
}

// hopByHopHeaders is the standard set defined in RFC 9110 section 7.6.1.
// These concern a single connection hop and must never be forwarded.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// StripHopByHop removes hop-by-hop headers from h. It first processes the
// Connection header to remove sender-nominated headers (RFC 9110 section
// 7.6.1), then removes the standard hop-by-hop set.
func StripHopByHop(h http.Header) {
	// "Connection: close, X-Custom" nominates X-Custom as hop-by-hop for this
	// hop. h["Connection"] reads the raw value slice without canonicalizing, so
	// every Connection line is enumerated.
	for _, connVal := range h["Connection"] {
		for _, name := range strings.Split(connVal, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for name := range hopByHopHeaders {
		h.Del(name)
	}
}

// ApplyHeaderRules runs each rule in order against h.
// "add" appends a value, "set" replaces all values, "remove" deletes the header.
func ApplyHeaderRules(h http.Header, rules []HeaderRule) {
	for _, r := range rules {
		switch r.Action {
		case "add":
			h.Add(r.Name, r.Value)
		case "set":
			h.Set(r.Name, r.Value)
		case "remove":
			h.Del(r.Name)
		}
	}
}
```

Read `StripHopByHop` as the two passes in order: the `Connection`-nomination loop first, deleting each sender-named header, then the static-set loop deleting the fixed eight (which includes `Connection` itself). `ApplyHeaderRules` is a straight switch over the action string, and because `http.Header` methods canonicalize names, a rule for `x-service` and one for `X-Service` address the same header.

### The runnable demo

The demo builds a header set mixing end-to-end headers, a `Connection` line nominating `X-Internal`, and two standard hop-by-hop headers. It strips, then applies a set/add/add/remove rule list, dumping the header map in sorted order at each stage so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"sort"

	"example.com/header-rewriting"
)

func dump(label string, h http.Header) {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Println(label)
	for _, name := range names {
		fmt.Printf("  %s: %v\n", name, h[name])
	}
}

func main() {
	h := http.Header{}
	h.Set("Host-Authorization", "keep-me")   // an end-to-end header
	h.Set("Authorization", "Bearer token")   // end-to-end
	h.Set("Connection", "close, X-Internal") // nominates X-Internal as hop-by-hop
	h.Set("X-Internal", "secret-hop-value")  // removed via nomination
	h.Set("Transfer-Encoding", "chunked")    // standard hop-by-hop
	h.Set("Keep-Alive", "timeout=5")         // standard hop-by-hop

	dump("inbound headers:", h)

	headers.StripHopByHop(h)
	dump("after StripHopByHop:", h)

	headers.ApplyHeaderRules(h, []headers.HeaderRule{
		{Action: "set", Name: "X-Service", Value: "payments"},
		{Action: "add", Name: "X-Tag", Value: "a"},
		{Action: "add", Name: "X-Tag", Value: "b"},
		{Action: "remove", Name: "Authorization"},
	})
	dump("after ApplyHeaderRules:", h)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
inbound headers:
  Authorization: [Bearer token]
  Connection: [close, X-Internal]
  Host-Authorization: [keep-me]
  Keep-Alive: [timeout=5]
  Transfer-Encoding: [chunked]
  X-Internal: [secret-hop-value]
after StripHopByHop:
  Authorization: [Bearer token]
  Host-Authorization: [keep-me]
after ApplyHeaderRules:
  Host-Authorization: [keep-me]
  X-Service: [payments]
  X-Tag: [a b]
```

After the strip, the nominated `X-Internal` is gone along with `Transfer-Encoding`, `Keep-Alive`, and `Connection` itself, while the end-to-end `Authorization` and `Host-Authorization` survive. After the rules, `X-Service` is set, the two `X-Tag` adds accumulate into a two-value slice, and the `remove` deletes `Authorization`.

### Tests

The tests pin each behavior: the full standard set is stripped while a custom header survives, a `Connection`-nominated header is removed, two separate `Connection` lines each take effect, and the three rule actions behave with order respected.

Create `headers_test.go`:

```go
package headers

import (
	"net/http"
	"testing"
)

func TestStripHopByHopStandardSet(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	for name := range hopByHopHeaders {
		h.Set(name, "value")
	}
	h.Set("X-Custom", "keep")
	StripHopByHop(h)

	for name := range hopByHopHeaders {
		if h.Get(name) != "" {
			t.Errorf("%s should be stripped", name)
		}
	}
	if h.Get("X-Custom") != "keep" {
		t.Error("X-Custom should be preserved")
	}
}

func TestStripHopByHopConnectionNominated(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Connection", "close, X-Nominated")
	h.Set("X-Nominated", "should-go")
	h.Set("X-Keep", "keep")
	StripHopByHop(h)

	if h.Get("X-Nominated") != "" {
		t.Error("X-Nominated should be stripped (nominated by Connection header)")
	}
	if h.Get("X-Keep") != "keep" {
		t.Error("X-Keep should be preserved")
	}
}

func TestStripHopByHopMultipleConnectionValues(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	// Two separate Connection header lines, each nominating a header.
	h.Add("Connection", "X-One")
	h.Add("Connection", "X-Two")
	h.Set("X-One", "a")
	h.Set("X-Two", "b")
	h.Set("X-Three", "c")
	StripHopByHop(h)

	if h.Get("X-One") != "" || h.Get("X-Two") != "" {
		t.Error("both nominated headers should be stripped")
	}
	if h.Get("X-Three") != "c" {
		t.Error("X-Three should be preserved")
	}
}

func TestApplyHeaderRulesAdd(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("X-Multi", "first")
	ApplyHeaderRules(h, []HeaderRule{{Action: "add", Name: "X-Multi", Value: "second"}})
	if vals := h["X-Multi"]; len(vals) != 2 || vals[1] != "second" {
		t.Errorf("X-Multi = %v, want [first second]", vals)
	}
}

func TestApplyHeaderRulesSet(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("X-Token", "old")
	ApplyHeaderRules(h, []HeaderRule{{Action: "set", Name: "X-Token", Value: "new"}})
	if h.Get("X-Token") != "new" {
		t.Errorf("X-Token = %q, want new", h.Get("X-Token"))
	}
}

func TestApplyHeaderRulesRemove(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("X-Secret", "sensitive")
	ApplyHeaderRules(h, []HeaderRule{{Action: "remove", Name: "X-Secret"}})
	if h.Get("X-Secret") != "" {
		t.Error("X-Secret should be removed")
	}
}

func TestApplyHeaderRulesOrder(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	// set then remove: the header ends up gone.
	ApplyHeaderRules(h, []HeaderRule{
		{Action: "set", Name: "X-Flag", Value: "on"},
		{Action: "remove", Name: "X-Flag"},
	})
	if h.Get("X-Flag") != "" {
		t.Error("X-Flag set-then-removed should be absent")
	}
}
```

## Review

The strip is correct when both passes run and run in the right order. `TestStripHopByHopStandardSet` confirms every fixed header is removed and an unrelated `X-Custom` is left untouched; `TestStripHopByHopConnectionNominated` confirms the dynamic pass removes a sender-named header; and `TestStripHopByHopMultipleConnectionValues` is the guard for the raw-slice read — it sends two distinct `Connection` lines, which `h.Get` would collapse to one but `h["Connection"]` enumerates fully. The rule applier is correct when actions compose in order: `TestApplyHeaderRulesOrder` sets then removes the same header and expects it absent, which only holds if the slice is walked front to back. The common mistake this module exists to prevent is forwarding `Transfer-Encoding` — folding it into the standard set and stripping unconditionally is what stops the upstream from re-interpreting already-decoded chunked framing.

## Resources

- [RFC 9110 section 7.6.1 — Connection and hop-by-hop headers](https://httpwg.org/specs/rfc9110.html#field.connection) — the authoritative definition of the static set and the `Connection`-nomination mechanism.
- [`net/http.Header`](https://pkg.go.dev/net/http#Header) — the `Add`, `Set`, `Del`, and `Get` semantics the rule applier relies on, including name canonicalization.
- [`strings.Split` and `strings.TrimSpace`](https://pkg.go.dev/strings#Split) — parsing a comma-separated `Connection` value into individual nominated names.

---

Back to [01-request-router.md](01-request-router.md) | Next: [03-http-reverse-proxy.md](03-http-reverse-proxy.md)
