# 4. Groups and Nested Attributes

Groups solve key collisions without giving up structure. This lesson builds a request logger that uses `slog.GroupAttrs` and `Logger.WithGroup`, then verifies how the grouped fields appear in deterministic text output.

## Concepts

### Inline Groups

`slog.Group` and `slog.GroupAttrs` collect several attributes under one key. `TextHandler` qualifies nested keys with dots, such as `request.method=GET`; `JSONHandler` emits nested objects. `GroupAttrs` accepts `slog.Attr` values directly and avoids the alternating key-value form.

### Persistent Groups

`Logger.WithGroup` returns a new logger whose later attributes are qualified by the group name. It is useful when a subsystem uses common names like `id`, `status`, or `duration` that might collide with fields from another subsystem.

### Schema Design

Logging schema is an API for operators. Group names should be stable and meaningful: `request.id`, `user.id`, and `response.status` are easier to query than repeated flat `id` keys.

## Exercises

Edit `go.mod`:

```go
module example.com/sloggroups

go 1.26
```

### Exercise 1: Build a Grouped Request Logger

Create `requestlog.go`:

```go
package requestlog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

var ErrNilLogger = errors.New("logger must not be nil")

type Logger struct {
	logger *slog.Logger
}

type Event struct {
	Method   string
	Path     string
	UserID   string
	Status   int
	Duration time.Duration
}

func New(logger *slog.Logger) (*Logger, error) {
	if logger == nil {
		return nil, fmt.Errorf("request logger: %w", ErrNilLogger)
	}
	return &Logger{logger: logger.WithGroup("http")}, nil
}

func (l *Logger) Logger() *slog.Logger {
	return l.logger
}

func (l *Logger) Completed(ctx context.Context, event Event) {
	if event.Status == 0 {
		event.Status = http.StatusOK
	}
	l.logger.LogAttrs(ctx, slog.LevelInfo, "request completed",
		slog.GroupAttrs("request",
			slog.String("method", event.Method),
			slog.String("path", event.Path),
		),
		slog.GroupAttrs("user",
			slog.String("id", event.UserID),
		),
		slog.GroupAttrs("response",
			slog.Int("status", event.Status),
			slog.Duration("duration", event.Duration),
		),
	)
}
```

### Exercise 2: Test Grouped Output

Create `requestlog_test.go`:

```go
package requestlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newTextLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

func TestCompletedWritesGroupedFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event Event
		want  []string
	}{
		{name: "explicit status", event: Event{Method: "GET", Path: "/users/42", UserID: "u-42", Status: 204, Duration: 15 * time.Millisecond}, want: []string{"http.request.method=GET", "http.request.path=/users/42", "http.user.id=u-42", "http.response.status=204", "http.response.duration=15ms"}},
		{name: "default status", event: Event{Method: "POST", Path: "/users", UserID: "u-99"}, want: []string{"http.response.status=200"}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger, err := New(newTextLogger(&buf))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			logger.Completed(context.Background(), tc.event)

			got := buf.String()
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

	_, err := New(nil)
	if !errors.Is(err, ErrNilLogger) {
		t.Fatalf("New(nil) error = %v, want ErrNilLogger", err)
	}
}

func ExampleLogger_Completed() {
	var buf bytes.Buffer
	logger, _ := New(newTextLogger(&buf))
	logger.Completed(context.Background(), Event{Method: "GET", Path: "/users/42", UserID: "u-42", Status: 200})
	fmt.Print(buf.String())
	// Output: level=INFO msg="request completed" http.request.method=GET http.request.path=/users/42 http.user.id=u-42 http.response.status=200 http.response.duration=0s
}
```

Your turn: add a test for a second path that proves `request.path` and `user.id` do not collide.

### Exercise 3: Add a Demo Command

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	"example.com/sloggroups"
)

func main() {
	var buf bytes.Buffer
	base := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	logger, err := requestlog.New(base)
	if err != nil {
		panic(err)
	}
	logger.Completed(context.Background(), requestlog.Event{Method: "GET", Path: "/users/42", UserID: "u-42", Status: 200, Duration: time.Millisecond})
	fmt.Print(buf.String())
}
```

## Common Mistakes

Wrong: log flat `id` fields for request, user, and order in one record.

What happens: the last field may be ambiguous or difficult to query.

Fix: use groups such as `request.id`, `user.id`, and `order.id`.

Wrong: create a group name that changes with data, such as `WithGroup(userID)`.

What happens: log schemas become unbounded and hard to index.

Fix: keep group names stable and put data in attributes.

Wrong: assume text and JSON display groups identically.

What happens: text uses dotted keys while JSON uses nested objects.

Fix: test the handler format you deploy.

## Verification

From `~/go-exercises/slog-groups`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one test of your own for grouped fields.

## Summary

- `slog.GroupAttrs` builds grouped attributes from explicit `slog.Attr` values.
- `Logger.WithGroup` qualifies every later attribute from that logger.
- Stable group names prevent key collisions and make logs queryable.
- Text and JSON handlers represent groups differently.

## What's Next

Next, attach persistent attributes with [Slog with Logger Enrichment](../05-slog-with-for-logger-enrichment/05-slog-with-logger-enrichment.md).

## Resources

- `slog.GroupAttrs` documentation: https://pkg.go.dev/log/slog#GroupAttrs
- `Logger.WithGroup` documentation: https://pkg.go.dev/log/slog#Logger.WithGroup
- `log/slog` groups overview: https://pkg.go.dev/log/slog#hdr-Groups
