# 7. Cookie and Session Management

Cookies let HTTP servers persist small pieces of state on clients. This lesson builds a reusable `sessions` package that sets, reads, and deletes a session cookie while keeping session data on the server.

## Concepts

The `net/http` package provides `http.Cookie`, `http.SetCookie`, and `(*http.Request).Cookie`. A session cookie should contain an unpredictable identifier, not sensitive user data. The server maps that identifier to session data in a store that can be invalidated on logout.

Use `HttpOnly` to keep browser JavaScript from reading the cookie, `SameSite` to reduce cross-site request risk, and `Secure` when serving over HTTPS. A negative `MaxAge` deletes a cookie by asking the user agent to remove it.

## Exercises

Create this module layout:

```text
cookie-sessions/
    go.mod
    sessions.go
    sessions_example_test.go
    sessions_test.go
    cmd/demo/main.go
```

Create `go.mod`:

```go
module example.com/cookie-sessions

go 1.26
```

Create `sessions.go`:

```go
package sessions

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const CookieName = "session_id"

var (
	ErrInvalidUsername = errors.New("invalid username")
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionID       = errors.New("session id generation failed")
)

type Session struct {
	Username  string
	CreatedAt time.Time
}

type Store struct {
	mu       sync.RWMutex
	sessions map[string]Session
}

func NewStore() *Store {
	return &Store{sessions: make(map[string]Session)}
}

func (s *Store) Create(username string) (string, Session, error) {
	if username == "" {
		return "", Session{}, fmt.Errorf("%w: empty username", ErrInvalidUsername)
	}
	id, err := newID()
	if err != nil {
		return "", Session{}, fmt.Errorf("%w: %v", ErrSessionID, err)
	}
	session := Session{Username: username, CreatedAt: time.Now().UTC()}

	s.mu.Lock()
	s.sessions[id] = session
	s.mu.Unlock()

	return id, session, nil
}

func (s *Store) Get(id string) (Session, error) {
	s.mu.RLock()
	session, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return Session{}, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	return session, nil
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

func LoginHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := r.URL.Query().Get("user")
		id, _, err := store.Create(username)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     CookieName,
			Value:    id,
			Path:     "/",
			MaxAge:   3600,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		fmt.Fprintf(w, "logged in as %s\n", username)
	}
}

func ProfileHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(CookieName)
		if err != nil {
			http.Error(w, "missing session", http.StatusUnauthorized)
			return
		}

		session, err := store.Get(cookie.Value)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, "welcome %s\n", session.Username)
	}
}

func LogoutHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(CookieName); err == nil {
			store.Delete(cookie.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		fmt.Fprintln(w, "logged out")
	}
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
```

Create `sessions_example_test.go`:

```go
package sessions_test

import (
	"fmt"

	"example.com/cookie-sessions"
)

func ExampleStore_Count() {
	store := sessions.NewStore()
	_, session, err := store.Create("alice")
	fmt.Println(session.Username, store.Count(), err == nil)

	// Output:
	// alice 1 true
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"log"
	"net/http"

	"example.com/cookie-sessions"
)

func main() {
	store := sessions.NewStore()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", sessions.LoginHandler(store))
	mux.HandleFunc("GET /profile", sessions.ProfileHandler(store))
	mux.HandleFunc("GET /logout", sessions.LogoutHandler(store))

	log.Fatal(http.ListenAndServe(":8080", mux))
}
```

Create `sessions_test.go`:

```go
package sessions

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStoreValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		username string
	}{
		{name: "empty", username: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := NewStore().Create(tt.username)
			if !errors.Is(err, ErrInvalidUsername) {
				t.Fatalf("expected ErrInvalidUsername, got %v", err)
			}
		})
	}
}

func TestLoginProfileLogoutFlow(t *testing.T) {
	t.Parallel()

	store := NewStore()
	loginReq := httptest.NewRequest(http.MethodGet, "/login?user=alice", nil)
	loginRec := httptest.NewRecorder()
	LoginHandler(store).ServeHTTP(loginRec, loginReq)

	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d", loginRec.Code)
	}
	cookies := loginRec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected cookie: %+v", cookies)
	}

	profileReq := httptest.NewRequest(http.MethodGet, "/profile", nil)
	profileReq.AddCookie(cookies[0])
	profileRec := httptest.NewRecorder()
	ProfileHandler(store).ServeHTTP(profileRec, profileReq)
	if profileRec.Code != http.StatusOK || !strings.Contains(profileRec.Body.String(), "alice") {
		t.Fatalf("profile response = %d %q", profileRec.Code, profileRec.Body.String())
	}

	logoutReq := httptest.NewRequest(http.MethodGet, "/logout", nil)
	logoutReq.AddCookie(cookies[0])
	logoutRec := httptest.NewRecorder()
	LogoutHandler(store).ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK || store.Count() != 0 {
		t.Fatalf("logout failed: code=%d count=%d", logoutRec.Code, store.Count())
	}
	if logoutRec.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatalf("logout cookie did not expire")
	}
}

func TestProfileRejectsInvalidSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		cookie *http.Cookie
	}{
		{name: "missing"},
		{name: "unknown", cookie: &http.Cookie{Name: CookieName, Value: "missing"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/profile", nil)
			if tt.cookie != nil {
				req.AddCookie(tt.cookie)
			}
			rec := httptest.NewRecorder()
			ProfileHandler(NewStore()).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d", rec.Code)
			}
		})
	}
}
```

## Common Mistakes

- Storing usernames, roles, or permissions directly in a cookie value.
- Using predictable session IDs such as counters or timestamps.
- Omitting `HttpOnly` on authentication cookies.
- Forgetting to delete server-side session data on logout.
- Testing handlers through a live server when `httptest` would be faster and deterministic.

## Verification

Run these commands from the module root:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

You built a concurrency-safe session store, generated random session IDs with `crypto/rand`, set cookies with `http.SetCookie`, read them with `Request.Cookie`, deleted them with `MaxAge: -1`, and tested the HTTP flow with `httptest`.

## What's Next

Next: [File Upload and Multipart Forms](../08-file-upload-and-multipart-forms/08-file-upload-and-multipart-forms.md).

## Resources

- [net/http Cookie](https://pkg.go.dev/net/http#Cookie)
- [net/http SetCookie](https://pkg.go.dev/net/http#SetCookie)
- [net/http Request.Cookie](https://pkg.go.dev/net/http#Request.Cookie)
- [OWASP Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)
