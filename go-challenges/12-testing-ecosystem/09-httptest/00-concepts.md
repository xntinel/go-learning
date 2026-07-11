# HTTP Testing with net/http/httptest — Concepts

Every backend service has two halves that talk HTTP, and `net/http/httptest` is
the seam that lets you test both without a real network. The *inbound* half is
your handlers and middleware: code that receives an `*http.Request` and writes to
an `http.ResponseWriter`. You drive it with an `httptest.ResponseRecorder`, an
in-memory `ResponseWriter` that needs no socket and no goroutine. The *outbound*
half is your service's own clients: the code that calls a payments API, an auth
provider, a downstream microservice. You drive it either with a real ephemeral
`httptest.Server` (wire-level fidelity on a loopback port) or with an injected
`http.RoundTripper` stub (deterministic failure injection, no server at all).
Almost every real service needs both, and the two techniques answer two different
questions: "does my handler do the right thing with this request?" and "does my
client do the right thing with this response?"

This file is the conceptual foundation for the ten independent exercises that
follow. Each exercise is a real service component — a greeting/health API, a
payments client, an auth-and-recovery middleware chain, an SSE stream, a
cert-pinning HTTPS client — with its own hermetic `*_test.go`. Read this once and
you have the model you need to reason through all of them.

## Concepts

### The recorder: an in-memory ResponseWriter

`httptest.NewRecorder()` returns a `*httptest.ResponseRecorder`. You build a
server-side request with `httptest.NewRequest(method, target, body)`, call your
handler directly with `handler(rec, req)`, and then read what it wrote off the
recorder. Its live fields are `rec.Code` (the status passed to `WriteHeader`,
defaulting to 200), `rec.Body` (a `*bytes.Buffer` holding everything the handler
`Write`d), and `rec.Header()` (the header map the handler mutated). This is the
fastest possible handler test: no port, no listener, no goroutine, no real
round-trip. It is the right tool for handler *logic* — status codes, JSON bodies,
validation branches, header setting.

But a recorder is not an HTTP round-trip. There is no chunked transfer encoding,
no real `Content-Length` negotiation, no connection or keep-alive semantics, no
TLS, no redirect following. When those behaviors are what you are testing —
streaming and flush timing, TLS trust, redirects, connection reuse — a recorder
silently buffers everything and gives you a false sense of coverage. For those,
reach for a real server.

### Result() is the snapshot; the live fields are not

The recorder's `Code`, `Body`, and `Header()` are mutated *live* as the handler
runs. After the handler returns, the correct way to inspect the response as a
client would have seen it is `rec.Result()`, which returns a real `*http.Response`
snapshot: `res.StatusCode`, `res.Header`, `res.Cookies()`, and `res.Body`. Two
consequences matter. First, `res.Header` is the snapshot taken when the handler
finished, and `res.Cookies()` parses the `Set-Cookie` headers into
`[]*http.Cookie` for you — reading the live `rec.Header()` map by hand does not.
Second, `Result().Body` is an `io.ReadCloser` you must `Close()`. The old
`rec.HeaderMap` field is deprecated in favor of `Result().Header`; do not assert
against it.

### NewServer: a real listener on an ephemeral port

`httptest.NewServer(handler)` starts a real `net/http` server on `127.0.0.1:0` —
an ephemeral port chosen by the kernel — and exposes it at `srv.URL`. Because the
port is ephemeral, tests never collide on `:8080` the way a hardcoded
`http.ListenAndServe(":8080", …)` would under parallel or CI runs. It is a real
server: a real `net.Listener` and a serving goroutine. That means you *must*
close it, or you leak both a goroutine and a socket. Prefer
`t.Cleanup(srv.Close)` over a bare `defer srv.Close()`: cleanup composes correctly
under subtests and keeps the module copy-safe when a case is lifted into a
`t.Run`.

`srv.Client()` returns an `*http.Client` preconfigured to talk to that server —
crucially, it already trusts the server's TLS certificate and rewrites the
special host so the client reaches the loopback address. Use `srv.Client()` (or
`http.Get(srv.URL + path)` for plaintext) rather than constructing your own
client from scratch.

### NewRequest is server-side; do not try to send it

`httptest.NewRequest` builds a request suitable for *feeding a handler*: it sets
`RemoteAddr`, gives the request a server-shaped `URL`, and is what you pass into
`handler(rec, req)`. It is not a request you send over the wire. For an actual
outbound call — against `srv.URL` or any real endpoint — use
`http.NewRequestWithContext(ctx, method, url, body)` and hand it to a client's
`Do`. Confusing the two ("why is `RemoteAddr` not set on my client request?",
"why can't I send this `NewRequest`?") is a common early mistake.

### The RoundTripper seam: a server-free transport stub

`http.Client` delegates the actual byte-pushing to its `Transport`, an
`http.RoundTripper` with one method: `RoundTrip(*http.Request) (*http.Response,
error)`. Swapping in your own `RoundTripper` lets you return any response or error
you like without a server: a `503`, a `429` with a `Retry-After`, a
`context.DeadlineExceeded`, a connection-refused error, a truncated body. This is
the ideal way to exercise retry/backoff logic deterministically — you script an
exact sequence of failures and successes. The trade-off is fidelity: a stub does
not test real header serialization, real status-line parsing, real connection
behavior. Rule of thumb: stub the `RoundTripper` to test *your client's reaction*
to responses; use a real `httptest.Server` to test that the bytes on the wire are
actually right. A stubbed `*http.Response` must be fully formed — set
`StatusCode`, a non-nil `Header`, and a non-nil `Body`
(`io.NopCloser(strings.NewReader(...))`), or reads panic and assertions mislead.

### Context propagation is testable

A request carries a `context.Context` via `r.Context()`. When the client
disconnects or the request deadline passes, that context is canceled, and a
well-behaved handler doing long work must `select` on `ctx.Done()` and
short-circuit — returning `503`/`504` — rather than burning resources on a result
nobody will read. This is directly testable: build a request whose context is
already canceled or carries a short deadline (`httptest.NewRequestWithContext` or
`req.WithContext`), and assert the handler returns early and does *not* call its
downstream dependency (a spy records zero calls). On the client side, canceling
the context passed to `Do` aborts the call mid-flight. In Go 1.24+, base test
contexts on `t.Context()` so they cancel automatically with the test's lifetime.

### Streaming requires a Flusher, and a recorder cannot prove it

Chunked/streaming responses (Server-Sent Events, long-poll, log tailing) work by
writing a piece, then calling `Flush()` on the `http.Flusher` the
`ResponseWriter` implements, so the bytes leave immediately instead of waiting for
the handler to return. At the unit level you can assert `rec.Flushed` became true.
But a recorder *buffers everything* — it can never demonstrate that events arrive
*incrementally*. To test progressive delivery you need a real `httptest.Server`
and a client that reads the body incrementally (e.g. a `bufio.Scanner`). A
streaming handler must also watch `r.Context().Done()`: when the client
disconnects, the producer goroutine has to stop, or it leaks for the life of the
process.

### Connection reuse depends on draining the body

HTTP keep-alive lets a client reuse a TCP connection for the next request, which
is a large throughput win in production clients. Reuse only happens if you fully
*drain and close* each response body: `io.Copy(io.Discard, resp.Body)` then
`resp.Body.Close()`. Close a large body without reading it and the transport
cannot return the connection to the pool, so it is thrown away and the next
request dials fresh — a silent, common performance bug. `net/http/httptrace`
makes this observable: attach an `httptrace.ClientTrace` whose `GotConn` hook
records `GotConnInfo.Reused`, and the "I forgot to drain the body" bug becomes a
failing assertion (`Reused == false` where you expected `true`).

### TLS testing without a real CA

`httptest.NewTLSServer` starts an HTTPS server with a freshly generated,
self-signed certificate. No public CA trusts it, and that is deliberate:
`srv.Client()` is preconfigured to trust exactly that cert, so the happy path
works, while a stock `http.DefaultClient.Get(srv.URL)` fails verification with an
"unknown authority" error — proving your production trust settings matter. The
correct response to that failure is never `InsecureSkipVerify: true`; it is to
build trust explicitly. `srv.Certificate()` returns the server's
`*x509.Certificate`, from which you build an `x509.CertPool`, set it as
`RootCAs` (or pin it) in a `tls.Config`, and wire that into a
`http.Transport.TLSClientConfig`. If you need HTTP/2 behavior, set
`srv.EnableHTTP2 = true` on an *unstarted* server (`NewUnstartedServer`) before
calling `StartTLS`.

### Hermeticity is the discipline

Every module in this lesson is copy-safe and independent: its own `go mod init`,
its own code, its own tests, no shared global state. Servers bind to ephemeral
ports; every server and every response body is closed via `t.Cleanup` or `defer`;
subtests use `t.Parallel()` where it is safe; and each module passes `gofmt -l`
(empty), `go vet`, and `go test -race`. A test that leaks a goroutine, a socket,
or a temp file is not done, even if its assertions pass.

## Common Mistakes

### Binding a test to a fixed real port

Wrong: `http.ListenAndServe(":8080", mux)` inside a test. Two tests, or two CI
jobs, collide on the port and flake.

Fix: `httptest.NewServer(mux)` binds `127.0.0.1:0`; the kernel picks a free port
and hands it back as `srv.URL`.

### Forgetting to close the server or the body

Wrong: `srv := httptest.NewServer(...)` with no close, or reading `resp.Body`
without closing it. Each leaks a goroutine and/or a socket, and an undrained body
disables keep-alive.

Fix: `t.Cleanup(srv.Close)` for the server (composes under subtests better than a
bare `defer`), and always `io.Copy(io.Discard, resp.Body)` then
`resp.Body.Close()` on every response.

### Treating the live recorder fields as a snapshot

Wrong: asserting on `rec.Code`/`rec.Header()` as if frozen, or reaching for the
deprecated `rec.HeaderMap`. The live fields mutate during the handler and the map
does not parse cookies.

Fix: call `res := rec.Result()`, assert on `res.StatusCode`, `res.Header.Get(...)`
and `res.Cookies()`, and `defer res.Body.Close()`.

### Confusing NewRequest with a client request

Wrong: trying to "send" an `httptest.NewRequest`, or expecting `RemoteAddr`/`TLS`
on it to look like a client's.

Fix: `httptest.NewRequest` is for feeding a handler. For an outbound call use
`http.NewRequestWithContext` against `srv.URL` and a client's `Do`.

### Reaching for InsecureSkipVerify against NewTLSServer

Wrong: `http.DefaultClient.Get(srv.URL)` fails with an x509 unknown-authority
error, so you set `InsecureSkipVerify: true` and move on — training yourself to
disable the very check production relies on.

Fix: use `srv.Client()`, or build an `x509.CertPool` from `srv.Certificate()` and
set it as `RootCAs`. Verify the failure is a certificate error, do not paper over
it.

### Not draining the body before Close

Wrong: `resp.Body.Close()` on a large response without reading it. The connection
cannot be pooled, so the next request dials a fresh socket. Throughput quietly
degrades under load.

Fix: `io.Copy(io.Discard, resp.Body)` before `Close`. `httptrace`'s
`GotConnInfo.Reused` lets you assert reuse actually happens.

### Testing streaming with only a recorder

Wrong: asserting an SSE handler "delivers events incrementally" against a
`ResponseRecorder`, which buffers everything and reveals nothing about timing.

Fix: unit-assert `rec.Flushed` for the flush contract, but prove incremental
delivery with a real `httptest.Server` and an incremental read (`bufio.Scanner`).

### Handlers that ignore r.Context()

Wrong: a handler that runs its full downstream work even after the client
canceled or the deadline passed — wasting resources and never returning
`503`/`504`.

Fix: `select` on `r.Context().Done()` around long work; test it by feeding a
canceled or short-deadline context and asserting the handler short-circuits with
zero downstream calls.

### Leaking a streaming producer goroutine

Wrong: a streaming handler starts a producer that never checks
`r.Context().Done()`, so on client disconnect it runs forever.

Fix: the producer loop must `select` on `r.Context().Done()` and return; the test
asserts the producer stops after the client cancels (guard with a timeout so a
leak fails loudly).

### A stub RoundTripper that returns a malformed response

Wrong: a `RoundTripper` stub returning a `*http.Response` with a nil `Body`
(panics on read) or an unset `StatusCode`/`Header`, producing misleading results.

Fix: populate the response fully —
`&http.Response{StatusCode: 200, Header: make(http.Header), Body:
io.NopCloser(strings.NewReader(...))}`.

Next: [01-recorder-unit-handler.md](01-recorder-unit-handler.md)
