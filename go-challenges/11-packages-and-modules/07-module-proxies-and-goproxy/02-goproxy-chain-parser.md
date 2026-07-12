# Exercise 2: Parse a GOPROXY chain and decide failover on a per-proxy response

The single most consequential `GOPROXY` detail is the difference between a comma
and a pipe. This exercise builds the parser and the failover decision so you can
answer, from first principles, why a 500 from the corporate proxy does or does not
reach the public one.

## What you'll build

```text
goproxychain/              independent module: example.com/goproxychain
  go.mod                   go 1.26
  chain.go                 Parse, Entry/Sep, Outcome/Classify, Decide + sentinels
  cmd/
    demo/
      main.go              runs four failover scenarios end to end
  chain_test.go            table-driven Parse/Classify/Decide tests
  example_test.go          ExampleDecide with // Output
```

- Files: `chain.go`, `cmd/demo/main.go`, `chain_test.go`, `example_test.go`.
- Implement: `Parse(goproxy) []Entry` that records each entry's following separator, `Classify(status, err) Outcome`, and `Decide(chain, outcomes) (int, error)` returning the serving entry or a terminal sentinel error.
- Test: comma hard-stops on 500 but continues past 404; pipe continues past 500/timeout; `direct`/`off` terminate; mixed comma/pipe in one value; error classification asserted with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The two separators are a security control

`GOPROXY` is an ordered list, but the separator between two entries is not
cosmetic. A comma means "fall through to the next entry only on HTTP 404 or 410" —
i.e. only when the module is genuinely absent. A pipe means "fall through on ANY
error", including a 500, a TLS failure, or a timeout. That difference decides
whether a transient failure of a private proxy leaks a private module fetch to the
public internet. With `https://corp.example.com,https://proxy.golang.org`, a 500
from the corporate proxy hard-stops the build inside the perimeter. With
`https://corp.example.com|https://proxy.golang.org`, that same 500 falls through
and the fetch escapes.

The parser must therefore preserve which separator follows each entry, not just
the list of entries. We model each entry with the separator that comes after it;
the final entry gets `SepNone`. `direct` and `off` are terminal keywords: `direct`
fetches from version control, `off` means cache-only with no network.

`Decide` then walks the chain applying the rule per entry: on a 200 the entry
serves; on a 404/410 we fall through regardless of separator (both comma and pipe
treat "not found" as a reason to try the next proxy); on a hard error (5xx or a
timeout) we fall through only if the following separator is a pipe, otherwise we
hard-stop and return a wrapped `ErrHardStop`. Reaching `direct` serves from VCS;
reaching `off` returns `ErrOffline`. The three terminal outcomes are distinct
sentinel errors so a caller can tell "contained a private fetch" (`ErrHardStop`)
apart from "genuinely absent" (`ErrNotFound`) apart from "offline" (`ErrOffline`).

Create `chain.go`:

```go
// Package goproxychain parses a GOPROXY value into an ordered chain and decides
// which entry serves a module given each proxy's outcome, honoring the distinct
// comma and pipe failover semantics.
package goproxychain

import (
	"errors"
	"fmt"
	"net/http"
)

// Sep is the separator that FOLLOWS an entry in the chain and therefore governs
// whether an error at that entry falls through to the next one.
type Sep int

const (
	// SepNone marks the terminal entry: there is nothing to fall through to.
	SepNone Sep = iota
	// SepComma falls through to the next entry only on HTTP 404/410.
	SepComma
	// SepPipe falls through to the next entry on ANY error.
	SepPipe
)

func (s Sep) String() string {
	switch s {
	case SepComma:
		return ","
	case SepPipe:
		return "|"
	default:
		return ""
	}
}

// Entry is one element of a GOPROXY chain: a proxy URL or a terminal keyword,
// plus the separator that follows it.
type Entry struct {
	Value string
	Sep   Sep
}

// IsDirect reports whether the entry is the terminal "direct" keyword (fetch from
// version control).
func (e Entry) IsDirect() bool { return e.Value == "direct" }

// IsOff reports whether the entry is the terminal "off" keyword (cache only, no
// network).
func (e Entry) IsOff() bool { return e.Value == "off" }

// Parse splits a GOPROXY value into an ordered chain, recording each entry's
// following separator. Empty tokens are skipped.
func Parse(goproxy string) []Entry {
	var entries []Entry
	start := 0
	flush := func(end int, sep Sep) {
		tok := goproxy[start:end]
		if tok != "" {
			entries = append(entries, Entry{Value: tok, Sep: sep})
		}
		start = end + 1
	}
	for i := 0; i < len(goproxy); i++ {
		switch goproxy[i] {
		case ',':
			flush(i, SepComma)
		case '|':
			flush(i, SepPipe)
		}
	}
	flush(len(goproxy), SepNone)
	return entries
}

// Outcome is the classified result of contacting one proxy.
type Outcome int

const (
	OutcomeOK Outcome = iota
	OutcomeNotFound
	OutcomeServerError
	OutcomeTimeout
)

func (o Outcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeNotFound:
		return "not-found"
	case OutcomeServerError:
		return "server-error"
	default:
		return "timeout"
	}
}

// Classify maps an HTTP status (and any transport error) to an Outcome.
func Classify(status int, err error) Outcome {
	if err != nil {
		return OutcomeTimeout
	}
	switch status {
	case http.StatusOK:
		return OutcomeOK
	case http.StatusNotFound, http.StatusGone:
		return OutcomeNotFound
	default:
		return OutcomeServerError
	}
}

// Sentinel terminal errors, wrapped with %w so callers assert them via errors.Is.
var (
	// ErrNotFound means the module was 404/410 through the whole chain.
	ErrNotFound = errors.New("module not found in any proxy")
	// ErrHardStop means a non-404 error hit a comma boundary and did not fall
	// through, so the fetch was contained.
	ErrHardStop = errors.New("chain hard-stopped on a non-404 error")
	// ErrOffline means the chain reached the "off" keyword without serving.
	ErrOffline = errors.New("GOPROXY=off: module not in cache")
)

// Decide walks the chain and returns the index of the entry that serves the
// module, or a terminal error. outcomes maps an entry index to its proxy result;
// terminal keyword entries ignore it.
func Decide(chain []Entry, outcomes map[int]Outcome) (int, error) {
	for i, e := range chain {
		switch {
		case e.IsOff():
			return -1, ErrOffline
		case e.IsDirect():
			return i, nil
		default:
			switch outcomes[i] {
			case OutcomeOK:
				return i, nil
			case OutcomeNotFound:
				// Both comma and pipe fall through on 404/410.
				if e.Sep == SepNone {
					return -1, ErrNotFound
				}
				continue
			default:
				// A hard error (5xx or timeout) falls through only on a pipe.
				if e.Sep == SepPipe {
					continue
				}
				return -1, fmt.Errorf("%w: entry %d (%s) returned %s", ErrHardStop, i, e.Value, outcomes[i])
			}
		}
	}
	return -1, ErrNotFound
}
```

### The runnable demo

The demo scripts the four scenarios that separate a leak-safe chain from a
leaky one: a comma chain that contains a corporate 500, a comma chain that
correctly falls through a 404, a pipe chain that falls through a transient 500,
and a bare `off`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/goproxychain"
)

func main() {
	cases := []struct {
		name     string
		goproxy  string
		outcomes map[int]goproxychain.Outcome
	}{
		{
			name:    "comma: corp 500 does not leak to public",
			goproxy: "https://corp.example.com,https://proxy.golang.org,direct",
			outcomes: map[int]goproxychain.Outcome{
				0: goproxychain.OutcomeServerError,
			},
		},
		{
			name:    "comma: corp 404 falls through to public",
			goproxy: "https://corp.example.com,https://proxy.golang.org,direct",
			outcomes: map[int]goproxychain.Outcome{
				0: goproxychain.OutcomeNotFound,
				1: goproxychain.OutcomeOK,
			},
		},
		{
			name:    "pipe: transient 500 falls through to public",
			goproxy: "https://corp.example.com|https://proxy.golang.org,direct",
			outcomes: map[int]goproxychain.Outcome{
				0: goproxychain.OutcomeServerError,
				1: goproxychain.OutcomeOK,
			},
		},
		{
			name:     "off: nothing served, offline",
			goproxy:  "off",
			outcomes: map[int]goproxychain.Outcome{},
		},
	}

	for _, c := range cases {
		chain := goproxychain.Parse(c.goproxy)
		idx, err := goproxychain.Decide(chain, c.outcomes)
		fmt.Printf("%s\n", c.name)
		fmt.Printf("  chain: %d entries\n", len(chain))
		if err != nil {
			fmt.Printf("  result: error: %v\n", err)
		} else {
			fmt.Printf("  result: served by entry %d (%s)\n", idx, chain[idx].Value)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
comma: corp 500 does not leak to public
  chain: 3 entries
  result: error: chain hard-stopped on a non-404 error: entry 0 (https://corp.example.com) returned server-error
comma: corp 404 falls through to public
  chain: 3 entries
  result: served by entry 1 (https://proxy.golang.org)
pipe: transient 500 falls through to public
  chain: 3 entries
  result: served by entry 1 (https://proxy.golang.org)
off: nothing served, offline
  chain: 1 entries
  result: error: GOPROXY=off: module not in cache
```

### Tests

The table proves each rule in isolation: parsing preserves separators, `Classify`
maps status codes and transport errors, and `Decide` distinguishes fall-through
from hard-stop. The terminal classification is asserted with `errors.Is` against
the sentinels so a refactor of the message string cannot silently change which
condition fired.

Create `chain_test.go`:

```go
package goproxychain

import (
	"errors"
	"net/http"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		goproxy string
		want    []Entry
	}{
		{
			name:    "comma separated with direct terminal",
			goproxy: "https://corp.example.com,https://proxy.golang.org,direct",
			want: []Entry{
				{"https://corp.example.com", SepComma},
				{"https://proxy.golang.org", SepComma},
				{"direct", SepNone},
			},
		},
		{
			name:    "pipe separated",
			goproxy: "https://a|https://b",
			want: []Entry{
				{"https://a", SepPipe},
				{"https://b", SepNone},
			},
		},
		{
			name:    "mixed comma and pipe",
			goproxy: "https://a|https://b,https://c",
			want: []Entry{
				{"https://a", SepPipe},
				{"https://b", SepComma},
				{"https://c", SepNone},
			},
		},
		{
			name:    "single off keyword",
			goproxy: "off",
			want:    []Entry{{"off", SepNone}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Parse(tt.goproxy)
			if len(got) != len(tt.want) {
				t.Fatalf("Parse(%q) = %v; want %v", tt.goproxy, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("entry %d = %+v; want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status int
		err    error
		want   Outcome
	}{
		{http.StatusOK, nil, OutcomeOK},
		{http.StatusNotFound, nil, OutcomeNotFound},
		{http.StatusGone, nil, OutcomeNotFound},
		{http.StatusInternalServerError, nil, OutcomeServerError},
		{http.StatusBadGateway, nil, OutcomeServerError},
		{0, errors.New("dial timeout"), OutcomeTimeout},
	}
	for _, tt := range tests {
		if got := Classify(tt.status, tt.err); got != tt.want {
			t.Errorf("Classify(%d,%v) = %v; want %v", tt.status, tt.err, got, tt.want)
		}
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		goproxy   string
		outcomes  map[int]Outcome
		wantIndex int
		wantErr   error
	}{
		{
			name:      "comma hard-stops on 500 (no leak to public)",
			goproxy:   "https://corp.example.com,https://proxy.golang.org,direct",
			outcomes:  map[int]Outcome{0: OutcomeServerError},
			wantIndex: -1,
			wantErr:   ErrHardStop,
		},
		{
			name:      "comma continues past a 404",
			goproxy:   "https://corp.example.com,https://proxy.golang.org,direct",
			outcomes:  map[int]Outcome{0: OutcomeNotFound, 1: OutcomeOK},
			wantIndex: 1,
		},
		{
			name:      "pipe continues past a 500",
			goproxy:   "https://corp.example.com|https://proxy.golang.org,direct",
			outcomes:  map[int]Outcome{0: OutcomeServerError, 1: OutcomeOK},
			wantIndex: 1,
		},
		{
			name:      "pipe continues past a timeout",
			goproxy:   "https://corp.example.com|https://proxy.golang.org,direct",
			outcomes:  map[int]Outcome{0: OutcomeTimeout, 1: OutcomeOK},
			wantIndex: 1,
		},
		{
			name:      "direct terminates and serves",
			goproxy:   "https://corp.example.com,direct",
			outcomes:  map[int]Outcome{0: OutcomeNotFound},
			wantIndex: 1,
		},
		{
			name:      "off terminates with offline error",
			goproxy:   "off",
			outcomes:  map[int]Outcome{},
			wantIndex: -1,
			wantErr:   ErrOffline,
		},
		{
			name:      "404 through the whole chain",
			goproxy:   "https://a,https://b",
			outcomes:  map[int]Outcome{0: OutcomeNotFound, 1: OutcomeNotFound},
			wantIndex: -1,
			wantErr:   ErrNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idx, err := Decide(Parse(tt.goproxy), tt.outcomes)
			if idx != tt.wantIndex {
				t.Errorf("index = %d; want %d", idx, tt.wantIndex)
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("err = %v; want errors.Is %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Errorf("unexpected err: %v", err)
			}
		})
	}
}
```

Create `example_test.go`:

```go
package goproxychain

import "fmt"

func ExampleDecide() {
	chain := Parse("https://corp.example.com,https://proxy.golang.org,direct")
	_, err := Decide(chain, map[int]Outcome{0: OutcomeServerError})
	fmt.Println(err)
	// Output: chain hard-stopped on a non-404 error: entry 0 (https://corp.example.com) returned server-error
}
```

## Review

The parser is correct when the separator stored on each entry is the one that
FOLLOWS it and the last entry is `SepNone`. The decision is correct when 404/410
always falls through (both separators), a hard error falls through only on a pipe,
and the three terminal conditions map to three distinct sentinel errors. The trap
to avoid is treating comma and pipe as interchangeable: the whole security value
of a comma chain is that a 500 does NOT fall through, so `ErrHardStop` firing on
the comma-500 case is the property under test, not an inconvenience. Assert the
terminal condition with `errors.Is`, never by string-matching the message, and run
`go test -race`.

## Resources

- [Go Modules Reference: GOPROXY protocol and fallback](https://go.dev/ref/mod#goproxy-protocol) — the 404/410 fallback rule and the comma/pipe semantics.
- [Go Modules Reference: Environment variables (GOPROXY)](https://go.dev/ref/mod#environment-variables) — the definition of the chain and the `direct`/`off` keywords.
- [`net/http` status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusOK`, `StatusNotFound`, `StatusGone`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-effective-goenv-resolver.md](01-effective-goenv-resolver.md) | Next: [03-proxy-path-codec.md](03-proxy-path-codec.md)
