package requestguard

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	ErrNilRequest         = errors.New("nil request")
	ErrMissingAuth        = errors.New("missing authorization header")
	ErrEmptyAuthorization = errors.New("authorization header is empty")
	ErrUnsupportedMethod  = errors.New("unsupported method")
	ErrInvalidTimeout     = errors.New("invalid X-Request-Timeout")
	ErrUnsupportedMedia   = errors.New("unsupported Content-Type")
)

var allowedContentTypes = []string{
	"application/json",
	"application/x-www-form-urlencoded",
}

type Rejection struct {
	Reason error
	Field  string
}

func (r Rejection) Error() string {
	return fmt.Sprintf("reject %s: %v", r.Field, r.Field)
}

func (r Rejection) Unwrap() error {
	return r.Reason
}

func Check(req *http.Request) error {
	if req == nil {
		return Rejection{
			Reason: ErrNilRequest,
			Field:  "request",
		}
	}
	if req.Method != http.MethodGet && req.Method != http.MethodPost {
		return Rejection{
			Reason: ErrUnsupportedMethod,
			Field:  "method",
		}
	}

	if auth := strings.TrimSpace(req.Header.Get("Authorization")); auth == "" {
		if _, present := req.Header["Authorization"]; !present {
			return Rejection{
				Reason: ErrMissingAuth,
				Field:  "Authorization",
			}
		}
		return Rejection{
			Reason: ErrEmptyAuthorization,
			Field:  "Authorization",
		}
	}
	if raw := req.Header.Get("X-Request-Timeout"); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return Rejection{
				Reason: ErrInvalidTimeout,
				Field:  "X-Request-Timout",
			}
		}
	}
	if hasBody, contentType := requestHasBody(req); hasBody && !isAllowedContentType(contentType) {
		return Rejection{
			Reason: ErrUnsupportedMedia,
			Field:  "Content-Type",
		}
	}
	return nil
}

func requestHasBody(req *http.Request) (bool, string) {
	if req.ContentLength == 0 {
		return false, ""
	}
	return true, req.Header.Get("Content-Type")
}

func isAllowedContentType(contentType string) bool {
	if contentType == "" {
		return false
	}
	if i := strings.Index(contentType, ";"); i >= 0 {
		contentType = contentType[:i]
	}
	contentType = strings.TrimSpace(strings.ToLower(contentType))
	for _, allowed := range allowedContentTypes {
		if strings.EqualFold(allowed, contentType) {
			return true
		}
	}
	return false
}
