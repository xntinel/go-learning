# 9. gRPC Service

Real gRPC uses generated protobuf messages, generated service interfaces, HTTP/2, status codes, deadlines, and streaming. This offline lesson models the core shape with a standard-library `taskrpc` package: requests, responses, a service interface, an in-process client, context handling, sentinel errors, and a tiny binary request/response boundary.

## Concepts

### RPC Is a Boundary

An RPC call separates client code from service implementation. Generated gRPC code usually owns marshaling and dispatch; this lesson writes a small boundary by hand so the service shape is testable without network-only dependencies.

### Context Belongs on Service Methods

`context.Context` carries cancellation and deadlines. Even an in-process service should check `ctx.Err()` before doing work, because real generated handlers receive a context.

### Typed Errors Stand In for Status Codes

Real gRPC uses `codes.NotFound` and `status.Error`. Offline, sentinel errors such as `ErrNotFound` and `ErrInvalidRequest` preserve the same caller behavior through `errors.Is`.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/18-encoding-json-xml-protobuf/09-grpc-service/09-grpc-service/cmd/demo
cd go-solutions/18-encoding-json-xml-protobuf/09-grpc-service/09-grpc-service
go mod edit -go=1.26
```

### Exercise 1: Build the Service and Client

Create `task.go`:

```go
package taskrpc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrNotFound       = errors.New("task not found")
	ErrInvalidRequest = errors.New("invalid request")
	ErrInvalidWire    = errors.New("invalid wire data")
	ErrTruncated      = errors.New("truncated wire data")
)

type Status string

const (
	StatusOpen Status = "open"
	StatusDone Status = "done"
)

type Task struct {
	id          uint64
	title       string
	description string
	status      Status
}

func (t Task) ID() uint64          { return t.id }
func (t Task) Title() string       { return t.title }
func (t Task) Description() string { return t.description }
func (t Task) Status() Status      { return t.status }

type CreateTaskRequest struct {
	Title       string
	Description string
}

type GetTaskRequest struct{ ID uint64 }

type UpdateStatusRequest struct {
	ID     uint64
	Status Status
}

type TaskService interface {
	CreateTask(context.Context, CreateTaskRequest) (Task, error)
	GetTask(context.Context, GetTaskRequest) (Task, error)
	UpdateStatus(context.Context, UpdateStatusRequest) (Task, error)
}

type InMemoryService struct {
	mu     sync.Mutex
	nextID uint64
	tasks  map[uint64]Task
}

func NewInMemoryService() *InMemoryService {
	return &InMemoryService{nextID: 1, tasks: make(map[uint64]Task)}
}

func (s *InMemoryService) CreateTask(ctx context.Context, req CreateTaskRequest) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	if req.Title == "" {
		return Task{}, fmt.Errorf("title is required: %w", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task := Task{id: s.nextID, title: req.Title, description: req.Description, status: StatusOpen}
	s.tasks[task.id] = task
	s.nextID++
	return task, nil
}

func (s *InMemoryService) GetTask(ctx context.Context, req GetTaskRequest) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[req.ID]
	if !ok {
		return Task{}, fmt.Errorf("id %d: %w", req.ID, ErrNotFound)
	}
	return task, nil
}

func (s *InMemoryService) UpdateStatus(ctx context.Context, req UpdateStatusRequest) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	if req.Status != StatusOpen && req.Status != StatusDone {
		return Task{}, fmt.Errorf("status %q: %w", req.Status, ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[req.ID]
	if !ok {
		return Task{}, fmt.Errorf("id %d: %w", req.ID, ErrNotFound)
	}
	task.status = req.Status
	s.tasks[req.ID] = task
	return task, nil
}

type Client struct{ service TaskService }

func NewClient(service TaskService) Client { return Client{service: service} }

func (c Client) CreateTask(ctx context.Context, title, description string) (Task, error) {
	req, err := decodeCreateTaskRequest(encodeCreateTaskRequest(CreateTaskRequest{Title: title, Description: description}))
	if err != nil {
		return Task{}, fmt.Errorf("decode create request: %w", err)
	}
	task, err := c.service.CreateTask(ctx, req)
	if err != nil {
		return Task{}, err
	}
	return decodeTask(encodeTask(task))
}

func (c Client) GetTask(ctx context.Context, id uint64) (Task, error) {
	task, err := c.service.GetTask(ctx, GetTaskRequest{ID: id})
	if err != nil {
		return Task{}, err
	}
	return decodeTask(encodeTask(task))
}

func (c Client) UpdateStatus(ctx context.Context, id uint64, status Status) (Task, error) {
	task, err := c.service.UpdateStatus(ctx, UpdateStatusRequest{ID: id, Status: status})
	if err != nil {
		return Task{}, err
	}
	return decodeTask(encodeTask(task))
}

func encodeCreateTaskRequest(req CreateTaskRequest) []byte {
	var out []byte
	out = appendStringField(out, 1, req.Title)
	out = appendStringField(out, 2, req.Description)
	return out
}

func decodeCreateTaskRequest(data []byte) (CreateTaskRequest, error) {
	var req CreateTaskRequest
	for len(data) > 0 {
		field, wire, rest, err := readKey(data)
		if err != nil {
			return CreateTaskRequest{}, err
		}
		value, next, err := readStringField(wire, rest)
		if err != nil {
			return CreateTaskRequest{}, fmt.Errorf("create field %d: %w", field, err)
		}
		switch field {
		case 1:
			req.Title = value
		case 2:
			req.Description = value
		default:
			return CreateTaskRequest{}, fmt.Errorf("create field %d: %w", field, ErrInvalidWire)
		}
		data = next
	}
	return req, nil
}

func encodeTask(task Task) []byte {
	var out []byte
	out = appendVarintField(out, 1, task.id)
	out = appendStringField(out, 2, task.title)
	out = appendStringField(out, 3, task.description)
	out = appendStringField(out, 4, string(task.status))
	return out
}

func decodeTask(data []byte) (Task, error) {
	var task Task
	for len(data) > 0 {
		field, wire, rest, err := readKey(data)
		if err != nil {
			return Task{}, err
		}
		switch field {
		case 1:
			if wire != 0 {
				return Task{}, fmt.Errorf("task id: %w", ErrInvalidWire)
			}
			value, next, err := readVarint(rest)
			if err != nil {
				return Task{}, fmt.Errorf("task id: %w", err)
			}
			task.id = value
			data = next
		case 2:
			value, next, err := readStringField(wire, rest)
			if err != nil {
				return Task{}, fmt.Errorf("task title: %w", err)
			}
			task.title = value
			data = next
		case 3:
			value, next, err := readStringField(wire, rest)
			if err != nil {
				return Task{}, fmt.Errorf("task description: %w", err)
			}
			task.description = value
			data = next
		case 4:
			value, next, err := readStringField(wire, rest)
			if err != nil {
				return Task{}, fmt.Errorf("task status: %w", err)
			}
			task.status = Status(value)
			data = next
		default:
			return Task{}, fmt.Errorf("task field %d: %w", field, ErrInvalidWire)
		}
	}
	return task, nil
}

func appendKey(out []byte, field, wire uint64) []byte {
	return binary.AppendUvarint(out, field<<3|wire)
}
func appendVarintField(out []byte, field, value uint64) []byte {
	return binary.AppendUvarint(appendKey(out, field, 0), value)
}
func appendStringField(out []byte, field uint64, value string) []byte {
	out = appendKey(out, field, 2)
	out = binary.AppendUvarint(out, uint64(len(value)))
	return append(out, value...)
}

func readKey(data []byte) (uint64, uint64, []byte, error) {
	key, rest, err := readVarint(data)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("key: %w", err)
	}
	field := key >> 3
	wire := key & 7
	if field == 0 {
		return 0, 0, nil, fmt.Errorf("field zero: %w", ErrInvalidWire)
	}
	return field, wire, rest, nil
}

func readStringField(wire uint64, data []byte) (string, []byte, error) {
	if wire != 2 {
		return "", nil, ErrInvalidWire
	}
	size, rest, err := readVarint(data)
	if err != nil {
		return "", nil, err
	}
	if size > uint64(len(rest)) {
		return "", nil, ErrTruncated
	}
	return string(rest[:size]), rest[size:], nil
}

func readVarint(data []byte) (uint64, []byte, error) {
	value, n := binary.Uvarint(data)
	if n > 0 {
		return value, data[n:], nil
	}
	if n == 0 {
		return 0, nil, ErrTruncated
	}
	return 0, nil, ErrInvalidWire
}
```

### Exercise 2: Test RPC Behavior and Wire Failures

Create `task_test.go`:

```go
package taskrpc

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClientCallsService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		title       string
		description string
		status      Status
	}{
		{name: "open task", title: "write codec", description: "cover encoding", status: StatusOpen},
		{name: "completed task", title: "ship rpc", description: "mark done", status: StatusDone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := NewClient(NewInMemoryService())
			task, err := client.CreateTask(context.Background(), tt.title, tt.description)
			if err != nil {
				t.Fatal(err)
			}
			task, err = client.UpdateStatus(context.Background(), task.ID(), tt.status)
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.GetTask(context.Background(), task.ID())
			if err != nil {
				t.Fatal(err)
			}
			if got.Title() != tt.title || got.Description() != tt.description || got.Status() != tt.status {
				t.Fatalf("task = %#v", got)
			}
		})
	}
}

func TestServiceErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(Client) error
		want error
	}{
		{name: "missing title", run: func(client Client) error {
			_, err := client.CreateTask(context.Background(), "", "missing title")
			return err
		}, want: ErrInvalidRequest},
		{name: "missing task", run: func(client Client) error { _, err := client.GetTask(context.Background(), 99); return err }, want: ErrNotFound},
		{name: "invalid status", run: func(client Client) error {
			task, err := client.CreateTask(context.Background(), "x", "y")
			if err != nil {
				return err
			}
			_, err = client.UpdateStatus(context.Background(), task.ID(), Status("blocked"))
			return err
		}, want: ErrInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.run(NewClient(NewInMemoryService()))
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestWireErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
		want error
	}{
		{name: "truncated key", data: []byte{0x80}, want: ErrTruncated},
		{name: "field zero", data: []byte{0x00}, want: ErrInvalidWire},
		{name: "wrong id wire", data: []byte{0x0a, 0x00}, want: ErrInvalidWire},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeTask(tt.data)
			if !errors.Is(err, tt.want) {
				t.Fatalf("decodeTask() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func ExampleClient_CreateTask() {
	client := NewClient(NewInMemoryService())
	task, _ := client.CreateTask(context.Background(), "learn rpc", "use an in-process service")
	fmt.Println(task.ID(), task.Title(), task.Status())
	// Output:
	// 1 learn rpc open
}
```

Your turn: add a test using a cancelled context and assert the returned error matches `context.Canceled`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/taskrpc"
)

func main() {
	client := taskrpc.NewClient(taskrpc.NewInMemoryService())
	task, err := client.CreateTask(context.Background(), "write service", "model unary rpc without dependencies")
	if err != nil {
		log.Fatal(err)
	}
	task, err = client.UpdateStatus(context.Background(), task.ID(), taskrpc.StatusDone)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d %s %s\n", task.ID(), task.Title(), task.Status())
}
```

## Common Mistakes

- Wrong: importing generated gRPC packages when the environment cannot download dependencies. What happens: verification cannot run offline. Fix: model the service boundary with stdlib only here.
- Wrong: omitting `context.Context` from service methods. What happens: cancellation cannot be represented. Fix: accept and check context in each method.
- Wrong: returning untyped strings for not-found cases. What happens: callers cannot branch safely. Fix: wrap `ErrNotFound`.

## Verification

From `~/go-exercises/taskrpc`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All commands must pass. Add at least one test of your own before considering the lesson complete.

## Summary

- gRPC is a generated RPC boundary; this lesson models that boundary offline.
- Service methods should accept `context.Context`.
- Sentinel errors can stand in for status classes in dependency-free exercises.
- A client wrapper keeps request and response encoding separate from service logic.

## What's Next

Next: [Binary Encoding](../10-binary-encoding/10-binary-encoding.md).

## Resources

- [context package documentation](https://pkg.go.dev/context)
- [encoding/binary package documentation](https://pkg.go.dev/encoding/binary)
- [errors.Is documentation](https://pkg.go.dev/errors#Is)
- [gRPC core concepts](https://grpc.io/docs/what-is-grpc/core-concepts/)
