# 13. Building a REST API

Build a complete task API with only the Go standard library. The hard part is keeping HTTP routing, JSON decoding, validation errors, and repository state separate enough that handler tests can prove the API contract without binding a real port.

## Concepts

The standard library is enough for a small REST API. `net/http` provides `http.Handler`, `http.HandlerFunc`, `http.NewServeMux`, method-aware patterns such as `"GET /tasks/{id}"`, status constants, and request path variables through `Request.PathValue`. `encoding/json` provides stream-oriented request decoding and response encoding. `net/http/httptest` provides in-process server request and response objects so tests can exercise handlers without binding ports.

This lesson builds a small task API as a library package plus a demo command. The library owns the data model, repository, service methods, HTTP routing, JSON helpers, and tests. The command only wires the handler to `http.Server`, which keeps the API easy to test.

Errors cross package boundaries as values, not strings. Repository and validation functions return sentinel errors wrapped with context by using `fmt.Errorf("...: %w", ErrNotFound)`. The handler uses `errors.Is` to choose HTTP status codes without depending on exact error text.

## Exercises

Create this module.

```go
// go.mod
module example.com/taskapi

go 1.26
```

Create the library package.

```go
// taskapi.go
package taskapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound     = errors.New("task not found")
	ErrInvalidInput = errors.New("invalid task input")
)

type Status string

const (
	StatusTodo Status = "todo"
	StatusDone Status = "done"
)

type Task struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type TaskInput struct {
	Title  string `json:"title"`
	Status Status `json:"status"`
}

type Store struct {
	mu     sync.RWMutex
	nextID int
	tasks  map[string]Task
	now    func() time.Time
}

func NewStore() *Store {
	return &Store{
		nextID: 1,
		tasks:  make(map[string]Task),
		now:    time.Now,
	}
}

func (s *Store) Create(input TaskInput) (Task, error) {
	if err := validate(input); err != nil {
		return Task{}, fmt.Errorf("create task: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	task := Task{
		ID:        fmt.Sprintf("task-%d", s.nextID),
		Title:     strings.TrimSpace(input.Title),
		Status:    input.Status,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.nextID++
	s.tasks[task.ID] = task
	return task, nil
}

func (s *Store) List(status Status) []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tasks := make([]Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		if status != "" && task.Status != status {
			continue
		}
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].ID < tasks[j].ID
	})
	return tasks
}

func (s *Store) Get(id string) (Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("get task %q: %w", id, ErrNotFound)
	}
	return task, nil
}

func (s *Store) Update(id string, input TaskInput) (Task, error) {
	if err := validate(input); err != nil {
		return Task{}, fmt.Errorf("update task %q: %w", id, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("update task %q: %w", id, ErrNotFound)
	}
	task.Title = strings.TrimSpace(input.Title)
	task.Status = input.Status
	task.UpdatedAt = s.now().UTC()
	s.tasks[id] = task
	return task, nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[id]; !ok {
		return fmt.Errorf("delete task %q: %w", id, ErrNotFound)
	}
	delete(s.tasks, id)
	return nil
}

func validate(input TaskInput) error {
	if strings.TrimSpace(input.Title) == "" {
		return fmt.Errorf("title is required: %w", ErrInvalidInput)
	}
	if input.Status != StatusTodo && input.Status != StatusDone {
		return fmt.Errorf("status must be todo or done: %w", ErrInvalidInput)
	}
	return nil
}

type Handler struct {
	store *Store
}

func NewHandler(store *Store) http.Handler {
	if store == nil {
		store = NewStore()
	}
	h := &Handler{store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tasks", h.listTasks)
	mux.HandleFunc("POST /tasks", h.createTask)
	mux.HandleFunc("GET /tasks/{id}", h.getTask)
	mux.HandleFunc("PUT /tasks/{id}", h.updateTask)
	mux.HandleFunc("DELETE /tasks/{id}", h.deleteTask)
	return mux
}

func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	status := Status(r.URL.Query().Get("status"))
	if status != "" && status != StatusTodo && status != StatusDone {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "status must be todo or done")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": h.store.List(status)})
}

func (h *Handler) createTask(w http.ResponseWriter, r *http.Request) {
	input, ok := decodeInput(w, r)
	if !ok {
		return
	}
	task, err := h.store.Create(input)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	w.Header().Set("Location", "/tasks/"+task.ID)
	writeJSON(w, http.StatusCreated, task)
}

func (h *Handler) getTask(w http.ResponseWriter, r *http.Request) {
	task, err := h.store.Get(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *Handler) updateTask(w http.ResponseWriter, r *http.Request) {
	input, ok := decodeInput(w, r)
	if !ok {
		return
	}
	task, err := h.store.Update(r.PathValue("id"), input)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *Handler) deleteTask(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Delete(r.PathValue("id")); err != nil {
		writeMappedError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeInput(w http.ResponseWriter, r *http.Request) (TaskInput, bool) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return TaskInput{}, false
	}
	defer r.Body.Close()

	var input TaskInput
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "request body must be a valid task JSON object")
		return TaskInput{}, false
	}
	return input, true
}

func writeMappedError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", ErrNotFound.Error())
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
```

Add an executable demo that imports the library instead of duplicating handler code.

```go
// cmd/demo/main.go
package main

import (
	"errors"
	"log"
	"net/http"
	"time"

	"example.com/taskapi"
)

func main() {
	server := &http.Server{
		Addr:              ":8080",
		Handler:           taskapi.NewHandler(taskapi.NewStore()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("listening on http://localhost:8080")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
```

Add a package example with an `Output` comment. Examples are tests, so this must stay deterministic.

```go
// example_test.go
package taskapi_test

import (
	"fmt"
	"log"

	"example.com/taskapi"
)

func ExampleStore_Create() {
	store := taskapi.NewStore()
	task, err := store.Create(taskapi.TaskInput{Title: "ship API", Status: taskapi.StatusTodo})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(task.ID, task.Title, task.Status)

	// Output:
	// task-1 ship API todo
}
```

Add handler and service tests. The handler tests use `httptest.NewRequest`, `httptest.NewRecorder`, table-driven cases, and parallel subtests. The service test proves wrapped sentinel errors remain detectable with `errors.Is`.

```go
// taskapi_test.go
package taskapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStoreWrapsSentinelErrors(t *testing.T) {
	t.Parallel()

	store := NewStore()
	_, err := store.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing error = %v, want ErrNotFound", err)
	}

	_, err = store.Create(TaskInput{Title: " ", Status: StatusTodo})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Create invalid error = %v, want ErrInvalidInput", err)
	}
}

func TestHandlerREST(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		contentType string
		seed        bool
		wantStatus  int
		wantBody    string
		wantHeader  string
	}{
		{
			name:        "create returns location",
			method:      http.MethodPost,
			path:        "/tasks",
			body:        `{"title":"write tests","status":"todo"}`,
			contentType: "application/json",
			wantStatus:  http.StatusCreated,
			wantBody:    `"id":"task-1"`,
			wantHeader:  "/tasks/task-1",
		},
		{
			name:       "create rejects missing json content type",
			method:     http.MethodPost,
			path:       "/tasks",
			body:       `{"title":"write tests","status":"todo"}`,
			wantStatus: http.StatusUnsupportedMediaType,
			wantBody:   "UNSUPPORTED_MEDIA_TYPE",
		},
		{
			name:       "get missing maps not found",
			method:     http.MethodGet,
			path:       "/tasks/missing",
			wantStatus: http.StatusNotFound,
			wantBody:   "NOT_FOUND",
		},
		{
			name:       "list filters seeded tasks",
			method:     http.MethodGet,
			path:       "/tasks?status=done",
			seed:       true,
			wantStatus: http.StatusOK,
			wantBody:   `"status":"done"`,
		},
		{
			name:        "put updates seeded task",
			method:      http.MethodPut,
			path:        "/tasks/task-1",
			body:        `{"title":"updated","status":"done"}`,
			contentType: "application/json",
			seed:        true,
			wantStatus:  http.StatusOK,
			wantBody:    `"title":"updated"`,
		},
		{
			name:       "delete removes seeded task",
			method:     http.MethodDelete,
			path:       "/tasks/task-1",
			seed:       true,
			wantStatus: http.StatusNoContent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := NewStore()
			if tc.seed {
				_, err := store.Create(TaskInput{Title: "seed", Status: StatusDone})
				if err != nil {
					t.Fatalf("seed task: %v", err)
				}
			}

			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rr := httptest.NewRecorder()

			NewHandler(store).ServeHTTP(rr, req)
			res := rr.Result()
			defer res.Body.Close()

			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}
			if res.Header.Get("Content-Type") != "application/json" && tc.wantStatus != http.StatusNoContent {
				t.Fatalf("Content-Type = %q, want application/json", res.Header.Get("Content-Type"))
			}
			if tc.wantHeader != "" && res.Header.Get("Location") != tc.wantHeader {
				t.Fatalf("Location = %q, want %q", res.Header.Get("Location"), tc.wantHeader)
			}

			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if tc.wantBody != "" && !strings.Contains(string(body), tc.wantBody) {
				t.Fatalf("body = %s, want substring %q", body, tc.wantBody)
			}
			if tc.wantStatus == http.StatusNoContent && len(body) != 0 {
				t.Fatalf("delete body = %q, want empty", body)
			}
		})
	}
}
```

Run the demo, then exercise the API manually if you want an end-to-end check.

```bash
go run ./cmd/demo
```

```bash
curl -i -X POST http://localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"title":"write tests","status":"todo"}'
curl -i http://localhost:8080/tasks
curl -i http://localhost:8080/tasks/task-1
curl -i -X PUT http://localhost:8080/tasks/task-1 \
  -H 'Content-Type: application/json' \
  -d '{"title":"ship API","status":"done"}'
curl -i 'http://localhost:8080/tasks?status=done'
curl -i -X DELETE http://localhost:8080/tasks/task-1
```

## Verification

Run these checks before considering the exercise complete.

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Expected result:

- `gofmt -l .` prints nothing.
- `go vet ./...` exits successfully.
- `go build ./...` exits successfully.
- `go test -count=1 -race ./...` exits successfully, including the `ExampleStore_Create` output check.

## Summary

- `net/http` can route a REST API with method-aware `ServeMux` patterns and `Request.PathValue`.
- `encoding/json.Decoder` is appropriate for request bodies, especially with `DisallowUnknownFields`.
- `net/http/httptest` lets handler tests run quickly without a real listener.
- Sentinel errors should be wrapped with `%w` where context is added and checked with `errors.Is` at the HTTP boundary.

## What's Next

Next: [HTTP Client: Retry, Circuit Breaker, and Tracing](../14-http-client-retry-circuit-breaker-tracing/14-http-client-retry-circuit-breaker.md).

## Resources

- [net/http](https://pkg.go.dev/net/http)
- [http.ServeMux](https://pkg.go.dev/net/http#ServeMux)
- [http.Request.PathValue](https://pkg.go.dev/net/http#Request.PathValue)
- [encoding/json](https://pkg.go.dev/encoding/json)
- [json.Decoder.DisallowUnknownFields](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields)
- [net/http/httptest](https://pkg.go.dev/net/http/httptest)
- [httptest.NewRequest](https://pkg.go.dev/net/http/httptest#NewRequest)
- [httptest.ResponseRecorder](https://pkg.go.dev/net/http/httptest#ResponseRecorder)
