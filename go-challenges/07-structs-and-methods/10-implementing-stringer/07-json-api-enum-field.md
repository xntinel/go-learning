# Exercise 7: HTTP API Enum Field — MarshalJSON/UnmarshalJSON With Strict Validation

At the API boundary an enum must be a quoted string in both directions and must
reject anything that is not a known name — a number, an empty string, a typo. This
module implements `json.Marshaler`/`json.Unmarshaler` on `Status` with strict
validation and wires it into a real `http.Handler` decoded with `httptest`.

Self-contained module: own `go mod init`, code, demo, and tests.

## What you'll build

```text
statusapi/                  independent module: example.com/statusapi
  go.mod
  status.go                 Status enum; MarshalJSON/UnmarshalJSON; EchoHandler
  cmd/
    demo/
      main.go               drives the handler with httptest and prints results
  status_test.go            marshal quoting; reject 3/""/bogus; httptest 200 and 400
```

- Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
- Implement: `MarshalJSON` emitting the quoted name (reusing the `String()` table), `UnmarshalJSON` rejecting numbers, empty, and unknown names; an `EchoHandler` that decodes a request body with `DisallowUnknownFields` and echoes the parsed status.
- Test: `json.Marshal(status)` yields the quoted name and equals `strconv.Quote(status.String())`; `json.Unmarshal` rejects `3`, `""`, `"bogus"`; an `httptest` round-trip returns 200 for `"running"` and 400 for an invalid value.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/10-implementing-stringer/07-json-api-enum-field/cmd/demo
cd go-solutions/07-structs-and-methods/10-implementing-stringer/07-json-api-enum-field
```

### Why MarshalJSON here, not just MarshalText

Exercise 3 used `TextMarshaler`, which `encoding/json` also honors. Implementing
`json.Marshaler`/`json.Unmarshaler` directly is the choice when the type needs
JSON-specific control — a different quoting rule, a richer error message, or
behavior that must be exactly the API's contract and not a side effect of the text
codec. `MarshalJSON` returns the already-quoted bytes (`strconv.Quote` produces a
valid JSON string for ASCII names), reusing the same `statusNames` table as
`String()`, so the wire name and the log name are guaranteed identical.

`UnmarshalJSON` is where strictness lives. It first unmarshals the raw bytes into a
`string`; that single step rejects a JSON *number* (`3` fails "cannot unmarshal
number into Go value of type string") and a JSON `null`/object with a clear error.
Then it looks the name up in `statusValues`: an empty string and an unknown name
both miss and return the wrapped `ErrUnknownStatus`. The net effect is that only a
quoted, known status name is ever accepted — the enum's validity is enforced at the
edge, before any handler logic runs.

The handler decodes with `json.Decoder` and `DisallowUnknownFields()`, so a payload
carrying an unexpected field is a `400`, not a silently-ignored typo. Any decode
error — bad status, unknown field, malformed JSON — becomes `http.StatusBadRequest`;
a valid request echoes the parsed status back as JSON, proving the value survived
the round trip through the API as a name.

Create `status.go`:

```go
package statusapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// Status is a job lifecycle state carried over the API as a quoted name.
type Status uint8

const (
	StatusUnknown Status = iota
	StatusPending
	StatusRunning
	StatusSucceeded
	StatusFailed
)

// ErrUnknownStatus is returned (wrapped) for a name not in the table.
var ErrUnknownStatus = errors.New("unknown status")

var statusNames = map[Status]string{
	StatusUnknown:   "unknown",
	StatusPending:   "pending",
	StatusRunning:   "running",
	StatusSucceeded: "succeeded",
	StatusFailed:    "failed",
}

var statusValues = func() map[string]Status {
	m := make(map[string]Status, len(statusNames))
	for s, name := range statusNames {
		m[name] = s
	}
	return m
}()

func (s Status) String() string {
	if name, ok := statusNames[s]; ok {
		return name
	}
	return "Status(" + strconv.FormatUint(uint64(s), 10) + ")"
}

// MarshalJSON emits the quoted name, sharing the table with String().
func (s Status) MarshalJSON() ([]byte, error) {
	name, ok := statusNames[s]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownStatus, uint8(s))
	}
	return []byte(strconv.Quote(name)), nil
}

// UnmarshalJSON accepts only a quoted, known status name. A number, null, empty
// string, or unknown name is rejected.
func (s *Status) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return fmt.Errorf("status must be a JSON string: %w", err)
	}
	v, ok := statusValues[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownStatus, name)
	}
	*s = v
	return nil
}

type echoBody struct {
	Status Status `json:"status"`
}

// EchoHandler decodes {"status":"<name>"} strictly and echoes the parsed status.
// Any decode error is a 400.
func EchoHandler(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req echoBody
	if err := dec.Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(echoBody{Status: req.Status})
}
```

### The runnable demo

The demo drives the handler in-process with `httptest`, posting a valid and an
invalid body so both the 200 and 400 paths print.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/statusapi"
)

func post(body string) (int, string) {
	req := httptest.NewRequest(http.MethodPost, "/status", strings.NewReader(body))
	rec := httptest.NewRecorder()
	statusapi.EchoHandler(rec, req)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func main() {
	code, body := post(`{"status":"running"}`)
	fmt.Printf("valid   -> %d %s\n", code, body)

	code, _ = post(`{"status":3}`)
	fmt.Printf("number  -> %d\n", code)

	code, _ = post(`{"status":"bogus"}`)
	fmt.Printf("bogus   -> %d\n", code)

	code, _ = post(`{"status":"running","extra":true}`)
	fmt.Printf("unknown -> %d\n", code)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid   -> 200 {"status":"running"}
number  -> 400
bogus   -> 400
unknown -> 400
```

### Tests

`TestMarshalQuotesName` asserts the wire form is the quoted name and that it equals
`strconv.Quote(s.String())`, pinning marshal/`String` consistency.
`TestUnmarshalRejects` covers the three invalid inputs. `TestHandlerRoundTrip`
exercises the real handler through `httptest`, asserting 200 with the echoed status
for a valid body and 400 for each invalid one.

Create `status_test.go`:

```go
package statusapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestMarshalQuotesName(t *testing.T) {
	t.Parallel()
	for s := range statusNames {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", s, err)
		}
		want := strconv.Quote(s.String())
		if string(b) != want {
			t.Errorf("Marshal(%v) = %s, want %s", s, b, want)
		}
	}
}

func TestUnmarshalRejects(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`3`, `""`, `"bogus"`, `null`} {
		var s Status
		if err := json.Unmarshal([]byte(in), &s); err == nil {
			t.Errorf("Unmarshal(%s) accepted; want rejection", in)
		}
	}
}

func TestUnmarshalUnknownWrapsSentinel(t *testing.T) {
	t.Parallel()
	var s Status
	err := json.Unmarshal([]byte(`"bogus"`), &s)
	if !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("error = %v, want wrap of ErrUnknownStatus", err)
	}
}

func TestHandlerRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		body     string
		wantCode int
		wantBody string
	}{
		{"valid", `{"status":"running"}`, http.StatusOK, `{"status":"running"}`},
		{"number", `{"status":3}`, http.StatusBadRequest, ""},
		{"bogus", `{"status":"bogus"}`, http.StatusBadRequest, ""},
		{"unknownfield", `{"status":"running","x":1}`, http.StatusBadRequest, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/status", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			EchoHandler(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d (body %q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantBody != "" {
				if got := strings.TrimSpace(rec.Body.String()); got != tc.wantBody {
					t.Errorf("body = %q, want %q", got, tc.wantBody)
				}
			}
		})
	}
}

func ExampleStatus_MarshalJSON() {
	b, _ := json.Marshal(StatusFailed)
	fmt.Printf("%s\n", b)
	// Output: "failed"
}
```

## Review

The boundary is correct when only a quoted, known name crosses it and everything
else is a `400` with a message, never a silently-coerced zero. The two properties
that matter: `MarshalJSON` reuses the `String()` table, so the API and the logs
name a status identically; and `UnmarshalJSON` rejects the number by decoding into a
`string` first, which is the cheapest way to forbid the raw-`iota` form on input.
`DisallowUnknownFields` turns a client typo in a field name into a loud error rather
than a dropped value — a small strictness that catches real integration bugs.
Because the handler is an ordinary `http.HandlerFunc`, the `httptest` round trip
exercises the exact code that runs in production, not a stand-in.

## Resources

- [encoding/json: Marshaler and Unmarshaler](https://pkg.go.dev/encoding/json#Marshaler) — the interfaces `json` calls.
- [encoding/json: Decoder.DisallowUnknownFields](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields) — strict decoding.
- [net/http/httptest: NewRecorder / NewRequest](https://pkg.go.dev/net/http/httptest#NewRecorder) — testing a handler in-process.

---

Back to [06-sql-valuer-scanner-enum-column.md](06-sql-valuer-scanner-enum-column.md) | Next: [08-bytesize-human-formatter.md](08-bytesize-human-formatter.md)
