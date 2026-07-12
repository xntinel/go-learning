# Exercise 9: Decoding a One-Off Webhook Payload with Anonymous Structs

A webhook handler receives a provider-specific payload that no other part of your
system ever constructs, validates a couple of required fields, and replies with a
tiny acknowledgment. Neither shape deserves a package-level type — they exist only
inside this one handler. That is the legitimate home of the anonymous struct: a
local request DTO and a local response shape.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
webhook/                    independent module: example.com/webhook
  go.mod                    module example.com/webhook
  webhook.go                Handler() http.Handler decoding into a local anonymous struct
  cmd/
    demo/
      main.go               drive the handler with httptest; print status and body
  webhook_test.go           valid 200, missing-field 400, malformed 400, unknown-field 400
```

Files: `webhook.go`, `cmd/demo/main.go`, `webhook_test.go`.
Implement: `Handler() http.Handler` that decodes the request body into a local
anonymous struct with `DisallowUnknownFields`, validates required fields, and
writes a one-off anonymous-struct JSON response (200 on success, 400 on any input
problem).
Test: a valid payload yields 200 and the expected body; a missing required field
yields 400; malformed JSON yields 400; an unknown key yields 400; the anonymous
types never appear on the package's exported surface.
Verify: `go test -count=1 -race ./...`

### Why anonymous structs are exactly right here

The payload shape — an event name and a nested data object with an order id and
amount — is dictated by the provider and used in one place: this handler. Naming
it (`type ProviderWebhookPayload struct {...}`) would add an exported symbol that
no other code references and that would tempt reuse it does not deserve. Declaring
it inline as `var payload struct {...}` keeps it scoped to the function, which is
the whole point of anonymous structs: a one-off shape with no package-level name.
The same reasoning applies to the acknowledgment written back — a `received` flag
and the echoed order id — declared inline as the value passed to `json.Encode`.

The handler also shows the production decoding discipline that pairs well with a
local struct. `json.NewDecoder(r.Body)` with `DisallowUnknownFields()` rejects a
payload carrying keys you did not model, which catches provider version drift and
typos instead of silently dropping them. Any decode error, a missing required
field, or an unknown key all map to `400 Bad Request` with a small JSON error
body; a well-formed, complete payload maps to `200` with the acknowledgment. The
`Content-Type` header is set before `WriteHeader`, because headers must be written
before the status line is committed.

Create `webhook.go`:

```go
package webhook

import (
	"encoding/json"
	"net/http"
)

// Handler returns an http.Handler that decodes a provider webhook into a local
// anonymous struct, validates it, and replies with a local anonymous-struct ack.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Request DTO: anonymous, scoped to this handler.
		var payload struct {
			Event string `json:"event"`
			Data  struct {
				OrderID string `json:"order_id"`
				Amount  int    `json:"amount"`
			} `json:"data"`
		}

		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid payload")
			return
		}
		if payload.Event == "" || payload.Data.OrderID == "" {
			writeError(w, http.StatusBadRequest, "missing required fields")
			return
		}

		// Response shape: anonymous, scoped to this handler.
		resp := struct {
			Received bool   `json:"received"`
			OrderID  string `json:"order_id"`
		}{Received: true, OrderID: payload.Data.OrderID}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg})
}
```

### The runnable demo

The demo drives the handler with `httptest` for a valid payload and prints the
status and body a caller would see.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/webhook"
)

func main() {
	body := `{"event":"order.paid","data":{"order_id":"o-99","amount":500}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	rec := httptest.NewRecorder()

	webhook.Handler().ServeHTTP(rec, req)

	fmt.Printf("status: %d\n", rec.Code)
	fmt.Printf("body: %s", rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 200
body: {"received":true,"order_id":"o-99"}
```

### Tests

The tests drive the handler with `httptest` across the valid path and each
rejection: missing field, malformed JSON, and an unknown key rejected by
`DisallowUnknownFields`.

Create `webhook_test.go`:

```go
package webhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec
}

func TestValidPayloadReturns200(t *testing.T) {
	t.Parallel()

	rec := post(t, `{"event":"order.paid","data":{"order_id":"o-99","amount":500}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	want := `{"received":true,"order_id":"o-99"}` + "\n"
	if rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestMissingRequiredFieldReturns400(t *testing.T) {
	t.Parallel()

	// order_id is absent.
	rec := post(t, `{"event":"order.paid","data":{"amount":500}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestMalformedJSONReturns400(t *testing.T) {
	t.Parallel()

	rec := post(t, `{"event":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestUnknownFieldReturns400(t *testing.T) {
	t.Parallel()

	// "signature" is not a modeled key; DisallowUnknownFields rejects it.
	rec := post(t, `{"event":"order.paid","signature":"x","data":{"order_id":"o-1"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
```

## Review

The handler is correct when a well-formed, complete payload yields 200 with the
acknowledgment body, and every input problem — malformed JSON, a missing required
field, an unknown key — yields 400. The lesson is scope: the request and response
shapes are anonymous structs precisely because they live and die inside this one
handler and belong on no exported surface. The mistakes to avoid: promoting these
one-off shapes to package-level types that invite misuse; skipping
`DisallowUnknownFields`, which lets provider drift pass silently; and writing the
status line before setting `Content-Type`, which loses the header. Run
`go test -race`; the handler holds no shared state, so it is trivially safe, and
the flag keeps the module honest as it grows.

## Resources

- [encoding/json: Decoder.DisallowUnknownFields](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields) — strict decoding of request bodies.
- [net/http: Request](https://pkg.go.dev/net/http#Request) — the request body and handler contract.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRequest`/`NewRecorder` for handler tests.

---

Prev: [08-embedded-json-flattening.md](08-embedded-json-flattening.md) | Back to [00-concepts.md](00-concepts.md) | Next: [10-pointer-vs-value-embedding.md](10-pointer-vs-value-embedding.md)
