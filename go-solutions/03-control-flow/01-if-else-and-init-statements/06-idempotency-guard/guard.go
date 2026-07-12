package idempotency

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
)

type storedResponse struct {
	status int
	body   []byte
}

type store struct {
	mu    sync.Mutex
	items map[string]*reservation
}
type reservation struct {
	done chan struct{}
	resp storedResponse
}

func newStore() *store {
	return &store{
		items: make(map[string]*reservation),
	}
}

func (s *store) reserve(key string) (*reservation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.items[key]; ok {
		return r, false
	}
	r := &reservation{
		done: make(chan struct{}),
	}
	s.items[key] = r
	return r, true
}

func Guard(next http.Handler) http.Handler {
	s := newStore()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			http.Error(w, "missing Idempotency-Key", http.StatusBadRequest)
			return
		}
		res, first := s.reserve(key)
		if !first {
			<-res.done
			writeStored(w, res.resp)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		res.resp = storedResponse{
			status: rec.Code,
			body:   rec.Body.Bytes(),
		}
		close(res.done)
		writeStored(w, res.resp)
	})
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

func writeStored(w http.ResponseWriter, resp storedResponse) {
	w.WriteHeader(resp.status)
	_, _ = w.Write(bytes.Clone(resp.body))
}
