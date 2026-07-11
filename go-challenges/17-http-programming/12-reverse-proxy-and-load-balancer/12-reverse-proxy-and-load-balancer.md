# 12. Reverse Proxy and Load Balancer

Reverse proxies accept client traffic, choose an upstream service, forward the request, and copy the upstream response back to the client. This lesson builds a reusable `lbproxy` package using only the Go standard library: `net/http`, `net/http/httputil`, `net/http/httptest`, `net/url`, and synchronization primitives.

## Concepts

The `httputil.ReverseProxy` type is an `http.Handler` that proxies requests to another server. The official documentation recommends configuring new proxies with `Rewrite` rather than the deprecated `Director`. A `Rewrite` function receives a `*httputil.ProxyRequest`; call `SetURL` to route the outbound request to a backend and `SetXForwarded` to set `X-Forwarded-For`, `X-Forwarded-Host`, and `X-Forwarded-Proto`.

Health checks are ordinary HTTP requests. The `net/http` documentation states that clients and transports are safe for concurrent use, so the load balancer keeps one reusable `http.Client` for health probes. Tests use `httptest.NewServer` for real loopback HTTP servers and `httptest.NewRecorder` or server clients for handler behavior.

## Exercises

Create this module layout:

```text
reverse-proxy-load-balancer/
    go.mod
    lbproxy.go
    lbproxy_example_test.go
    lbproxy_test.go
    cmd/demo/main.go
```

Create `go.mod`:

```go
module example.com/reverse-proxy-load-balancer

go 1.26
```

Create `lbproxy.go`:

```go
package lbproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

var (
	ErrNoBackends        = errors.New("no backends")
	ErrInvalidBackendURL = errors.New("invalid backend url")
	ErrUnknownBackend    = errors.New("unknown backend")
	ErrNoHealthyBackends = errors.New("no healthy backends")
	ErrHealthCheckFailed = errors.New("health check failed")
)

type Backend struct {
	URL     *url.URL
	Healthy bool
}

type LoadBalancer struct {
	mu       sync.RWMutex
	backends []*Backend
	next     uint64
	client   *http.Client
}

func New(rawURLs []string) (*LoadBalancer, error) {
	if len(rawURLs) == 0 {
		return nil, fmt.Errorf("%w: provide at least one backend", ErrNoBackends)
	}

	lb := &LoadBalancer{
		client: &http.Client{Timeout: 2 * time.Second},
	}
	for _, raw := range rawURLs {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidBackendURL, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("%w: %q must include scheme and host", ErrInvalidBackendURL, raw)
		}
		lb.backends = append(lb.backends, &Backend{URL: u, Healthy: true})
	}

	return lb, nil
}

func (lb *LoadBalancer) NextBackend() (*url.URL, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	if len(lb.backends) == 0 {
		return nil, fmt.Errorf("%w: empty backend list", ErrNoBackends)
	}
	for range lb.backends {
		idx := int(lb.next % uint64(len(lb.backends)))
		lb.next++
		backend := lb.backends[idx]
		if backend.Healthy {
			return backend.URL, nil
		}
	}

	return nil, fmt.Errorf("%w: all backends are unhealthy", ErrNoHealthyBackends)
}

func (lb *LoadBalancer) SetHealthy(rawURL string, healthy bool) error {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	for _, backend := range lb.backends {
		if backend.URL.String() == rawURL {
			backend.Healthy = healthy
			return nil
		}
	}

	return fmt.Errorf("%w: %s", ErrUnknownBackend, rawURL)
}

func (lb *LoadBalancer) CheckOnce(ctx context.Context) error {
	lb.mu.RLock()
	backends := make([]*Backend, len(lb.backends))
	copy(backends, lb.backends)
	lb.mu.RUnlock()

	var checkErr error
	for _, backend := range backends {
		healthURL := backend.URL.ResolveReference(&url.URL{Path: "/health"})
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL.String(), nil)
		if err != nil {
			checkErr = errors.Join(checkErr, fmt.Errorf("%w: %w", ErrHealthCheckFailed, err))
			lb.setBackendHealth(backend.URL.String(), false)
			continue
		}

		resp, err := lb.client.Do(req)
		if err != nil {
			checkErr = errors.Join(checkErr, fmt.Errorf("%w: %w", ErrHealthCheckFailed, err))
			lb.setBackendHealth(backend.URL.String(), false)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		healthy := resp.StatusCode == http.StatusOK && closeErr == nil
		lb.setBackendHealth(backend.URL.String(), healthy)
		if !healthy {
			checkErr = errors.Join(checkErr, fmt.Errorf("%w: %s returned %s", ErrHealthCheckFailed, healthURL, resp.Status))
		}
	}

	return checkErr
}

func (lb *LoadBalancer) HealthCheck(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = lb.CheckOnce(ctx)
		}
	}
}

func (lb *LoadBalancer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, err := lb.NextBackend()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.SetXForwarded()
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				http.Error(w, err.Error(), http.StatusBadGateway)
			},
		}
		proxy.ServeHTTP(w, r)
	})
}

func (lb *LoadBalancer) setBackendHealth(rawURL string, healthy bool) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	for _, backend := range lb.backends {
		if backend.URL.String() == rawURL {
			backend.Healthy = healthy
			return
		}
	}
}
```

Create `lbproxy_example_test.go`:

```go
package lbproxy_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	lbproxy "example.com/reverse-proxy-load-balancer"
)

func ExampleLoadBalancer_Handler() {
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "backend-a")
	}))
	defer backendA.Close()

	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "backend-b")
	}))
	defer backendB.Close()

	lb, err := lbproxy.New([]string{backendA.URL, backendB.URL})
	if err != nil {
		panic(err)
	}
	proxy := httptest.NewServer(lb.Handler())
	defer proxy.Close()

	for range 2 {
		resp, err := proxy.Client().Get(proxy.URL)
		if err != nil {
			panic(err)
		}
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			panic(err)
		}
		fmt.Println(strings.TrimSpace(string(body)))
	}

	// Output:
	// backend-a
	// backend-b
}
```

Create `lbproxy_test.go`:

```go
package lbproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewReportsSentinelErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		urls []string
		want error
	}{
		{name: "empty", urls: nil, want: ErrNoBackends},
		{name: "missing host", urls: []string{"localhost:9000"}, want: ErrInvalidBackendURL},
		{name: "bad escape", urls: []string{"http://%zz"}, want: ErrInvalidBackendURL},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tc.urls)
			if !errors.Is(err, tc.want) {
				t.Fatalf("New() error = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestNextBackendRoundRobinAndHealth(t *testing.T) {
	t.Parallel()

	lb, err := New([]string{"http://one.test", "http://two.test", "http://three.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lb.SetHealthy("http://two.test", false); err != nil {
		t.Fatal(err)
	}

	var got []string
	for range 4 {
		u, err := lb.NextBackend()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, u.Host)
	}
	want := "one.test,three.test,one.test,three.test"
	if strings.Join(got, ",") != want {
		t.Fatalf("round robin = %q, want %q", strings.Join(got, ","), want)
	}
}

func TestHandlerProxiesRequestsAndForwardedHeaders(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/api/widgets" {
			t.Errorf("path = %q, want /api/widgets", got)
		}
		if got := r.Header.Get("X-Forwarded-For"); got == "" {
			t.Error("X-Forwarded-For is empty")
		}
		if got := r.Header.Get("X-Forwarded-Host"); got == "" {
			t.Error("X-Forwarded-Host is empty")
		}
		if got := r.Header.Get("X-Forwarded-Proto"); got != "http" {
			t.Errorf("X-Forwarded-Proto = %q, want http", got)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("proxied"))
	}))
	defer backend.Close()

	lb, err := New([]string{backend.URL})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(lb.Handler())
	defer proxy.Close()

	resp, err := proxy.Client().Get(proxy.URL + "/api/widgets")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if string(body) != "proxied" {
		t.Fatalf("body = %q, want proxied", string(body))
	}
}

func TestHandlerReturnsUnavailableWhenNoHealthyBackends(t *testing.T) {
	t.Parallel()

	lb, err := New([]string{"http://backend.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lb.SetHealthy("http://backend.test", false); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
	rec := httptest.NewRecorder()
	lb.Handler().ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), ErrNoHealthyBackends.Error()) {
		t.Fatalf("body = %q, want no healthy backends", string(body))
	}
}

func TestCheckOnceUpdatesBackendHealth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		healthCode  int
		wantHealthy bool
		wantErr     error
	}{
		{name: "healthy", healthCode: http.StatusOK, wantHealthy: true},
		{name: "unhealthy", healthCode: http.StatusInternalServerError, wantHealthy: false, wantErr: ErrHealthCheckFailed},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					w.WriteHeader(tc.healthCode)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer backend.Close()

			lb, err := New([]string{backend.URL})
			if err != nil {
				t.Fatal(err)
			}
			err = lb.CheckOnce(context.Background())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CheckOnce() error = %v, want errors.Is %v", err, tc.wantErr)
			}

			lb.mu.RLock()
			gotHealthy := lb.backends[0].Healthy
			lb.mu.RUnlock()
			if gotHealthy != tc.wantHealthy {
				t.Fatalf("healthy = %v, want %v", gotHealthy, tc.wantHealthy)
			}
		})
	}
}

func TestSetHealthyUnknownBackend(t *testing.T) {
	t.Parallel()

	lb, err := New([]string{"http://known.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lb.SetHealthy("http://missing.test", false); !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("SetHealthy() error = %v, want errors.Is ErrUnknownBackend", err)
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"

	lbproxy "example.com/reverse-proxy-load-balancer"
)

func main() {
	backendA := backend("backend-a")
	defer backendA.Close()
	backendB := backend("backend-b")
	defer backendB.Close()

	lb, err := lbproxy.New([]string{backendA.URL, backendB.URL})
	if err != nil {
		log.Fatal(err)
	}
	proxy := httptest.NewServer(lb.Handler())
	defer proxy.Close()

	for range 4 {
		resp, err := proxy.Client().Get(proxy.URL + "/demo")
		if err != nil {
			log.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(strings.TrimSpace(string(body)))
	}
}

func backend(name string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		fmt.Fprintln(w, name)
	}))
}
```

## Verification

Run these checks from the module root:

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Expected results:

- `gofmt -l .` prints nothing.
- `go vet ./...` exits successfully.
- `go build ./...` exits successfully.
- `go test -count=1 -race ./...` exits successfully.
- The example test prints `backend-a` then `backend-b`.
- The handler tests use `httptest` backends and verify proxying, forwarded headers, unavailable responses, health checks, table-driven subtests, `t.Parallel`, sentinel errors, `%w` wrapping, and `errors.Is`.

## Summary

`httputil.ReverseProxy` supplies the proxy machinery, while a small synchronized load balancer chooses healthy backends in round-robin order. `Rewrite`, `ProxyRequest.SetURL`, and `ProxyRequest.SetXForwarded` are the modern standard-library APIs for routing and forwarding headers. `httptest` makes proxy behavior testable without external services.

## Resources

- [net/http](https://pkg.go.dev/net/http)
- [net/http/httputil](https://pkg.go.dev/net/http/httputil)
- [net/http/httptest](https://pkg.go.dev/net/http/httptest)
