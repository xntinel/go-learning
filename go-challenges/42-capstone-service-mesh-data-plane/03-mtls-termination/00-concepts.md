# mTLS Termination — Concepts

The hard part of mutual TLS in a service-mesh proxy is not the handshake itself — `crypto/tls` handles that — it is the operational surface around it: rotating certificates without dropping connections, extracting service identities from SPIFFE URIs in the SAN, isolating trust per upstream backend, and detecting expiration before it causes an outage. This file is the conceptual foundation for the lesson. Read it once and you will have everything you need to reason through each exercise, which build those four concerns as independent, self-contained Go modules using only the standard library.

## What mTLS Adds Over TLS

In ordinary TLS the server authenticates to the client; the client is anonymous. In mutual TLS both sides present X.509 certificates and verify each other. In a service mesh the proxy sidecar performs two distinct TLS roles on every proxied request:

1. Downstream termination: the sidecar is the TLS server facing the calling service. It presents its own certificate and requires a client certificate (`tls.RequireAndVerifyClientCert`). After the handshake it extracts the caller's identity from the certificate's SAN.
2. Upstream origination: the sidecar is the TLS client facing the backend service. It presents its certificate as the client identity and verifies the backend's certificate against a per-backend CA pool.

The proxy never lets plaintext cross the wire between peers; it terminates downstream TLS and re-originates a fresh TLS session upstream. The two roles use two different extension points on `tls.Config` (`GetCertificate` for the server side, `GetClientCertificate` for the client side) and two different trust anchors (`ClientCAs` for verifying callers, `RootCAs` for verifying backends).

## Atomic Certificate Rotation

A mesh issues short-lived certificates and rotates them continuously, often hourly. The proxy must pick up a new certificate without restarting and without a window in which a handshake fails. The naive approach — mutating `tls.Config.Certificates` in place — is a data race: that slice is read by the handshake goroutine at connection time and writing it concurrently corrupts memory and trips the race detector.

`tls.Config` is created once and shared across many goroutines. The correct model is the callback fields, which are read fresh on every handshake:

- `GetCertificate` is called for every server-side TLS handshake; it returns the current certificate.
- `GetClientCertificate` is called for every client-side handshake when the server requests a certificate.

Both callbacks fire per connection, not once at startup, so they naturally serve the newest certificate to every new connection while in-flight connections keep their original certificate for their lifetime. Storing the certificate behind `atomic.Pointer[tls.Certificate]` makes the swap wait-free and race-detector clean:

```go
var store atomic.Pointer[tls.Certificate]
store.Store(cert)    // initial load
store.Store(newCert) // rotation: all new connections see newCert
```

The callback simply loads the pointer. There is no lock on the handshake path, so rotation never contends with connection setup. This is the substrate that exercise 1 builds.

## SPIFFE Identity Extraction

SPIFFE (Secure Production Identity Framework For Everyone) represents service identity as a URI of the form `spiffe://trust-domain/path`, for example `spiffe://cluster.local/ns/default/sa/frontend`. The URI appears in the X.509 SAN extension as a URI SAN; Go exposes it on `*x509.Certificate` as `cert.URIs []*url.URL`. Extraction is a linear scan for the first URI whose `Scheme` is `"spiffe"`:

```go
for _, u := range cert.URIs {
	if u.Scheme == "spiffe" {
		return u.String(), nil
	}
}
```

The trust domain is `u.Host`; the workload path is `u.Path`. Identity-based routing and authorization decisions can key on either. A certificate may carry no SPIFFE URI at all (a plain TLS leaf with only DNS SANs), so a robust identity extractor falls back to the first DNS SAN, then the Subject CommonName, and a caller that needs a SPIFFE ID specifically must check for the empty string. This precedence — SPIFFE ID over DNS SAN over Subject — is what exercise 2 implements.

## Per-Upstream Trust Bundles

Different backend services may be signed by different CAs. The proxy must not allow a certificate signed by backend A's CA to authenticate a connection to backend B; if it did, compromising A's CA would let an attacker impersonate B. The fix is to create a separate `*tls.Config` per upstream, each with its own `RootCAs *x509.CertPool`. The proxy's own certificate store is shared (the same proxy identity is presented to every backend), but the trust anchor is per-backend.

A trust bundle is built from DER-encoded CA certificates by parsing each with `x509.ParseCertificate` and adding it to a fresh `x509.NewCertPool()`. The downstream (server) side uses the bundle as `ClientCAs` to verify callers; the upstream (client) side uses a per-backend bundle as `RootCAs` to verify that backend. Relying on the system root pool instead is the classic failure: a mesh CA is not in the OS trust store, so every handshake fails with `x509: certificate signed by unknown authority`. Exercise 3 builds the bundle and the two config constructors and proves the per-backend isolation.

## Certificate Expiration Monitoring

A certificate that expires silently causes every new TLS handshake to fail with `x509: certificate has expired or is not yet valid`. Polling `time.Until(leaf.NotAfter)` and emitting a warning when that value drops below a configurable threshold (for example 48 hours) gives operators a rotation window before outages occur.

Parsing the leaf from the stored `tls.Certificate` is one round-trip. `cert.Certificate` is a `[][]byte`; element zero is always the leaf DER:

```go
leaf, err := x509.ParseCertificate(cert.Certificate[0])
remaining := time.Until(leaf.NotAfter)
```

A warner runs on a ticker in its own goroutine, but exposing a synchronous `CheckNow` makes the check testable without waiting for a tick. Exercise 4 builds the warner and tests both the firing and the silent cases.

## Common Mistakes

### Mutating tls.Config.Certificates After Startup

Wrong: populate `cfg.Certificates = []tls.Certificate{cert}` at startup, then replace `cfg.Certificates[0]` when a new certificate arrives.

What happens: the field is read concurrently by the TLS handshake goroutine and written by the rotation goroutine. The race detector fires; the program corrupts memory in production.

Fix: leave `cfg.Certificates` empty. Set `cfg.GetCertificate` (server) or `cfg.GetClientCertificate` (client) to the certificate-store callbacks. The callback is called once per handshake with no lock held by `crypto/tls`, so an `atomic.Pointer` swap is race-free.

### Verifying Client Certificates Against the System Root Pool

Wrong: set `cfg.ClientCAs = nil` (or omit it) and rely on the system root CAs to verify client certificates.

What happens: on most systems the system trust store contains only public CAs. A certificate issued by your mesh CA is not in that store, so every mTLS handshake fails with `x509: certificate signed by unknown authority`.

Fix: build an explicit `*x509.CertPool` from your mesh CA's DER bytes and assign it to `cfg.ClientCAs`. The `TrustBundle` constructor in exercise 3 does exactly this.

### Sharing One tls.Config Across All Upstream Backends

Wrong: create one `*tls.Config` with `RootCAs` set to a pool containing every CA, and use it for every upstream backend.

What happens: a certificate signed by backend A's CA is accepted for a connection to backend B. An attacker who compromises backend A's CA can impersonate backend B.

Fix: build one config per upstream with an isolated `RootCAs` pool. Each upstream verifies only against the CA that is supposed to sign it.

### Dereferencing tls.Certificate.Leaf Without Checking for Nil

Wrong: read `cert.Leaf.NotAfter` to check expiry.

What happens: `tls.Certificate.Leaf` is nil unless the certificate was loaded with `tls.LoadX509KeyPair` and the leaf was subsequently parsed by the caller. When the certificate is constructed by hand (as in tests or rotation code), `Leaf` is nil and the dereference panics.

Fix: always parse from `cert.Certificate[0]` (the raw DER of the leaf) with `x509.ParseCertificate`. That slice element is always set when the `tls.Certificate` is constructed correctly.

---

Next: [01-cert-rotation.md](01-cert-rotation.md)
