# 5. Request Body Parsing and Validation

HTTP APIs must decode request bodies carefully and validate decoded data before using it. This lesson builds a reusable package that checks content type, limits body size, rejects unknown JSON fields, validates input, and returns testable wrapped sentinel errors.

## Concepts

The official `encoding/json` package provides `json.NewDecoder`, whose `Decode` method reads the next JSON value from an `io.Reader`. `Decoder.DisallowUnknownFields` makes decoding fail when a JSON object contains keys that do not match exported struct fields. The official `net/http` package provides `http.MaxBytesReader`, which limits the bytes read from a request body and returns an `*http.MaxBytesError` when the limit is exceeded.

Separate parsing from validation. Parsing answers whether the request can be decoded into the expected shape. Validation answers whether decoded field values satisfy your business rules. Use sentinel errors, wrap them with `%w`, and check them with `errors.Is` so handlers can map failure categories to HTTP status codes.

Use `httptest` for handler tests. A handler test should send a request, record the response, and assert the status code and body without opening a network port.

## Exercises

Create this module layout:

```text
bodyparsing/
  go.mod
  users.go
  users_example_test.go
  users_test.go
  cmd/demo/main.go
```

Create `go.mod`:

```go
module example.com/bodyparsing

go 1.26
```

Create `users.go`:

```go
package bodyparsing

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
)

var (
	ErrInvalidJSON          = errors.New("invalid JSON")
	ErrValidation           = errors.New("validation failed")
	ErrBodyTooLarge         = errors.New("request body too large")
	ErrUnsupportedMediaType = errors.New("unsupported media type")
)

type CreateUserRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Age   int    `json:"age"`
}

type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Age   int    `json:"age"`
}

type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func ValidateCreateUser(req CreateUserRequest) ([]FieldError, error) {
	var fieldErrors []FieldError

	if strings.TrimSpace(req.Name) == "" {
		fieldErrors = append(fieldErrors, FieldError{Field: "name", Message: "name is required"})
	}
	if strings.TrimSpace(req.Email) == "" {
		fieldErrors = append(fieldErrors, FieldError{Field: "email", Message: "email is required"})
	}
	if req.Age < 0 || req.Age > 150 {
		fieldErrors = append(fieldErrors, FieldError{Field: "age", Message: "age must be between 0 and 150"})
	}

	if len(fieldErrors) > 0 {
		return fieldErrors, fmt.Errorf("validate create user: %w", ErrValidation)
	}
	return nil, nil
}

func DecodeCreateUser(w http.ResponseWriter, r *http.Request, maxBytes int64) (CreateUserRequest, []FieldError, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return CreateUserRequest{}, nil, fmt.Errorf("parse content type: %w", ErrUnsupportedMediaType)
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req CreateUserRequest
	if err := dec.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return CreateUserRequest{}, nil, fmt.Errorf("decode create user: %w", ErrBodyTooLarge)
		}
		return CreateUserRequest{}, nil, fmt.Errorf("decode create user: %w", ErrInvalidJSON)
	}

	fields, err := ValidateCreateUser(req)
	if err != nil {
		return CreateUserRequest{}, fields, err
	}
	return req, nil, nil
}

func CreateUserHandler(maxBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, fields, err := DecodeCreateUser(w, r, maxBytes)
		switch {
		case errors.Is(err, ErrUnsupportedMediaType):
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "content type must be application/json"})
			return
		case errors.Is(err, ErrBodyTooLarge):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		case errors.Is(err, ErrInvalidJSON):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		case errors.Is(err, ErrValidation):
			writeJSON(w, http.StatusUnprocessableEntity, map[string][]FieldError{"errors": fields})
			return
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
			return
		}

		user := User{ID: "1", Name: req.Name, Email: req.Email, Age: req.Age}
		writeJSON(w, http.StatusCreated, user)
	})
}

func NewRouter(maxBytes int64) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /users", CreateUserHandler(maxBytes))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
```

Create `users_example_test.go`:

```go
package bodyparsing_test

import (
	"fmt"

	"example.com/bodyparsing"
)

func ExampleValidateCreateUser() {
	fields, err := bodyparsing.ValidateCreateUser(bodyparsing.CreateUserRequest{
		Name:  "Alice",
		Email: "alice@example.com",
		Age:   30,
	})

	fmt.Println(err == nil)
	fmt.Println(len(fields))

	// Output:
	// true
	// 0
}
```

Create `users_test.go`:

```go
package bodyparsing

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateCreateUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		req       CreateUserRequest
		wantErr   bool
		wantCount int
	}{
		{name: "valid", req: CreateUserRequest{Name: "Alice", Email: "alice@example.com", Age: 30}},
		{name: "missing fields", req: CreateUserRequest{Age: -1}, wantErr: true, wantCount: 3},
		{name: "age too high", req: CreateUserRequest{Name: "Alice", Email: "alice@example.com", Age: 151}, wantErr: true, wantCount: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fields, err := ValidateCreateUser(tt.req)
			if tt.wantErr {
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("ValidateCreateUser error = %v, want ErrValidation", err)
				}
				if len(fields) != tt.wantCount {
					t.Fatalf("field error count = %d, want %d", len(fields), tt.wantCount)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateCreateUser returned error: %v", err)
			}
		})
	}
}

func TestCreateUserHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		body        string
		maxBytes    int64
		statusCode  int
		wantBody    string
	}{
		{name: "created", contentType: "application/json", body: `{"name":"Alice","email":"alice@example.com","age":30}`, maxBytes: 1024, statusCode: http.StatusCreated, wantBody: `"id":"1"`},
		{name: "validation", contentType: "application/json", body: `{"name":"","email":"","age":-1}`, maxBytes: 1024, statusCode: http.StatusUnprocessableEntity, wantBody: "name is required"},
		{name: "bad json", contentType: "application/json", body: `not json`, maxBytes: 1024, statusCode: http.StatusBadRequest, wantBody: "invalid JSON"},
		{name: "unknown field", contentType: "application/json", body: `{"name":"Alice","email":"alice@example.com","age":30,"extra":true}`, maxBytes: 1024, statusCode: http.StatusBadRequest, wantBody: "invalid JSON"},
		{name: "too large", contentType: "application/json", body: `{"name":"Alice","email":"alice@example.com","age":30}`, maxBytes: 10, statusCode: http.StatusRequestEntityTooLarge, wantBody: "request body too large"},
		{name: "wrong media type", contentType: "text/plain", body: `{"name":"Alice","email":"alice@example.com","age":30}`, maxBytes: 1024, statusCode: http.StatusUnsupportedMediaType, wantBody: "content type must be application/json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, "/users", bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			rec := httptest.NewRecorder()

			CreateUserHandler(tt.maxBytes).ServeHTTP(rec, req)

			res := rec.Result()
			defer res.Body.Close()

			if res.StatusCode != tt.statusCode {
				t.Fatalf("status = %d, want %d", res.StatusCode, tt.statusCode)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("body = %q, want substring %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestDecodeCreateUserErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		body        string
		maxBytes    int64
		wantErr     error
	}{
		{name: "invalid json", contentType: "application/json", body: `{`, maxBytes: 1024, wantErr: ErrInvalidJSON},
		{name: "too large", contentType: "application/json", body: `{"name":"Alice"}`, maxBytes: 4, wantErr: ErrBodyTooLarge},
		{name: "validation", contentType: "application/json", body: `{"name":"","email":"","age":-1}`, maxBytes: 1024, wantErr: ErrValidation},
		{name: "unsupported", contentType: "text/plain", body: `{}`, maxBytes: 1024, wantErr: ErrUnsupportedMediaType},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, "/users", bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			rec := httptest.NewRecorder()

			_, _, err := DecodeCreateUser(rec, req, tt.maxBytes)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("DecodeCreateUser error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"log"
	"net/http"

	"example.com/bodyparsing"
)

func main() {
	log.Println("listening on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", bodyparsing.NewRouter(1<<20)))
}
```

## Common Mistakes

Using `io.ReadAll` before decoding reads the entire body into memory. Prefer `json.NewDecoder(r.Body)` and enforce a size limit with `http.MaxBytesReader`.

Letting `encoding/json` ignore unknown fields can hide client bugs. Call `Decoder.DisallowUnknownFields` when your API contract should be strict.

Returning `400 Bad Request` for valid JSON with invalid field values loses useful meaning. Use `422 Unprocessable Entity` for validation failures and reserve `400 Bad Request` for malformed JSON or shape errors.

Comparing wrapped errors with `==` fails. Use `errors.Is(err, ErrValidation)` and the other sentinels.

## Verification

Run these commands from the module root:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

Robust request-body handling checks `Content-Type`, limits body size with `http.MaxBytesReader`, decodes with `json.Decoder`, rejects unknown fields when strictness is required, validates decoded values separately, and maps wrapped sentinel errors to clear HTTP responses.

## What's Next

Next: [HTTP Client Timeouts](../06-http-client-timeouts/06-http-client-timeouts.md).

## Resources

- [encoding/json Decoder](https://pkg.go.dev/encoding/json#Decoder)
- [Decoder.DisallowUnknownFields](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields)
- [http.MaxBytesReader](https://pkg.go.dev/net/http#MaxBytesReader)
- [http.MaxBytesError](https://pkg.go.dev/net/http#MaxBytesError)
- [httptest package](https://pkg.go.dev/net/http/httptest)
