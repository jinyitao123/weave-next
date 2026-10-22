package llmrouter

import "fmt"

type systemOneHTTPError struct {
	status             int
	retryAfter, detail string
}

func (e *systemOneHTTPError) Error() string {
	return fmt.Sprintf("systemone: HTTP %d: %s", e.status, e.detail)
}
func (e *systemOneHTTPError) HTTPStatus() int         { return e.status }
func (e *systemOneHTTPError) RetryAfterValue() string { return e.retryAfter }
