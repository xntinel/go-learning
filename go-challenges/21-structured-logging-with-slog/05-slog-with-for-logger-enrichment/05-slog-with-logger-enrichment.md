# 5. Slog with Logger Enrichment

Logger enrichment removes repetitive fields from call sites while keeping logs correlated. This lesson builds a service logger that uses `Logger.With` and a redacting `slog.LogValuer`.

## Concepts

### Persistent Attributes

`Logger.With` returns a new logger that carries additional attributes on every record. The original logger is unchanged. This is useful for service names, environment names, request IDs, and other stable context.

### Redaction with LogValuer

If a value implements `LogValue() slog.Value`, handlers use that representation. This lets a package expose useful structure while preventing accidental leakage of secrets such as email addresses or tokens.

### Passing Enriched Loggers

An enriched logger should be passed explicitly through constructors and request handlers. That keeps dependencies visible and lets tests capture exactly the logger used by the code under test.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/slog-enrichment
cd ~/go-exercises/slog-enrichment
go mod init example.com/slogenrichment
```

Edit `go.mod`:

```go
module example.com/slogenrichment

go 1.26
```

### Exercise 1: Build an Enriched Service Logger

Create `service.go`:

```go
package servicelog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

var ErrNilLogger = errors.New("logger must not be nil")

type User struct {
	ID    string
	Role  string
	Email string
}

func (u User) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", u.ID),
		slog.String("role", u.Role),
	)
}

type Service struct {
	logger *slog.Logger
}

func New(logger *slog.Logger, service, env string) (*Service, error) {
	if logger == nil {
		return nil, fmt.Errorf("service log: %w", ErrNilLogger)
	}
	return &Service{logger: logger.With(slog.String("service", service), slog.String("env", env))}, nil
}

func (s *Service) Logger() *slog.Logger {
	return s.logger
}

func (s *Service) ForRequest(requestID string) *Service {
	return &Service{logger: s.logger.With(slog.String("request_id", requestID))}
}

func (s *Service) Authorize(ctx context.Context, user User, action string) {
	s.logger.LogAttrs(ctx, slog.LevelInfo, "authorization checked",
		slog.Any("user", user),
		slog.String("action", action),
	)
}
```

### Exercise 2: Test Enrichment and Redaction

Create `service_test.go`:

```go
package servicelog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func textLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

func TestAuthorizeIncludesPersistentAttributes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requestID string
		want      []string
	}{
		{name: "base service", want: []string{"service=billing", "env=test", "user.id=u-1", "user.role=admin"}},
		{name: "request service", requestID: "req-123", want: []string{"request_id=req-123", "action=refund"}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			svc, err := New(textLogger(&buf), "billing", "test")
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if tc.requestID != "" {
				svc = svc.ForRequest(tc.requestID)
			}
			svc.Authorize(context.Background(), User{ID: "u-1", Role: "admin", Email: "alice@example.com"}, "refund")

			got := buf.String()
			if strings.Contains(got, "alice@example.com") {
				t.Fatalf("email leaked in log output: %q", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("output %q missing %q", got, want)
				}
			}
		})
	}
}

func TestNewRejectsNilLogger(t *testing.T) {
	t.Parallel()

	_, err := New(nil, "billing", "test")
	if !errors.Is(err, ErrNilLogger) {
		t.Fatalf("New(nil) error = %v, want ErrNilLogger", err)
	}
}

func ExampleService_ForRequest() {
	var buf bytes.Buffer
	svc, _ := New(textLogger(&buf), "billing", "test")
	svc.ForRequest("req-123").Authorize(context.Background(), User{ID: "u-1", Role: "admin", Email: "alice@example.com"}, "refund")
	fmt.Print(buf.String())
	// Output: level=INFO msg="authorization checked" service=billing env=test request_id=req-123 user.id=u-1 user.role=admin action=refund
}
```

Your turn: add a test proving `ForRequest` does not mutate the base service logger.

### Exercise 3: Add a Demo Command

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"example.com/slogenrichment"
)

func main() {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	svc, err := servicelog.New(logger, "billing", "demo")
	if err != nil {
		panic(err)
	}
	svc.ForRequest("req-123").Authorize(context.Background(), servicelog.User{ID: "u-1", Role: "admin", Email: "alice@example.com"}, "refund")
	fmt.Print(buf.String())
}
```

## Common Mistakes

Wrong: call `With` repeatedly at every log site with the same service fields.

What happens: boilerplate grows and fields become inconsistent.

Fix: enrich once at construction time and pass the enriched logger.

Wrong: log entire user structs with `slog.Any` but no `LogValuer`.

What happens: sensitive fields may appear in logs.

Fix: implement `LogValue` and expose only approved fields.

Wrong: store a logger in `context.Context` as the primary dependency mechanism.

What happens: dependencies become implicit and hard to test.

Fix: pass loggers explicitly; reserve context for request-scoped values such as IDs.

## Verification

From `~/go-exercises/slog-enrichment`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add a test that proves redaction remains in place when the logger is request-scoped.

## Summary

- `Logger.With` returns a new logger carrying persistent attributes.
- `slog.LogValuer` controls how a type appears in logs.
- Redaction belongs in the type or boundary that owns the sensitive data.
- Explicit logger injection keeps logging testable.

## What's Next

Next, implement handler wrappers in [Custom Slog Handler](../06-custom-slog-handler/06-custom-slog-handler.md).

## Resources

- `Logger.With` documentation: https://pkg.go.dev/log/slog#Logger.With
- `slog.LogValuer` documentation: https://pkg.go.dev/log/slog#LogValuer
- `log/slog` performance notes: https://pkg.go.dev/log/slog#hdr-Performance_considerations
