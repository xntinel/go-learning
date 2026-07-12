package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	idempotency "github.com/sentinel/go-learning/go-solutions/03-control-flow/01-if-else-and-init-statements/06-idempotency-guard"
)

func main() {
	var charges atomic.Int64

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := charges.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "charge #%d", n)
	})

	srv := httptest.NewServer(idempotency.Guard(handler))

	post := func(key string) string {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err.Error()
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	fmt.Println("key A first:", post("A"))
	fmt.Println("key A repeat:", post("A"))
	fmt.Println("key B first:", post("B"))
	fmt.Println("no key:", post(""))
	fmt.Println("total charges:", charges.Load())
}
