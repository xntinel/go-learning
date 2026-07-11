package main

import (
	"fmt"
	"net/http"
	"strings"

	requestguard "github.com/sentinel/go-learning/go-solutions/03-control-flow/01-if-else-and-init-statements/01-request-guard-check/guard"
)

func decide(req *http.Request) string {
	if err := requestguard.Check(req); err != nil {
		return "reject: " + err.Error()
	}
	return "admit"
}

func main() {
	ok, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/orders", nil)
	ok.Header.Set("Authorization", "Bearer token-123")
	fmt.Println(decide(ok))

	noAuth, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/orders", nil)
	fmt.Println(decide(noAuth))

	badMedia, _ := http.NewRequest(http.MethodPost, "https://api.example.com/v1/orders", strings.NewReader("<x/>"))
	badMedia.Header.Set("Authorization", "Bearer token-123")
	badMedia.Header.Set("Content-Type", "text/xml")
	badMedia.ContentLength = 4
	fmt.Println(decide(badMedia))
}
