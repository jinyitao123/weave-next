package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// apiClient talks to the load-test Weave over real HTTP. Tokens and
// credentials are never printed.
type apiClient struct {
	base  string
	token string
	hc    *http.Client
}

func newAPIClient(base string, maxConns int) *apiClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = maxConns
	tr.MaxIdleConnsPerHost = maxConns
	tr.MaxConnsPerHost = 0
	return &apiClient{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 120 * time.Second, Transport: tr}}
}

type httpError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s %s -> HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

var secretPattern = regexp.MustCompile(`(wv_cap_|wv_sk_|eyJ|lt-)[A-Za-z0-9._\-]+`)

func redact(s string) string {
	s = secretPattern.ReplaceAllString(s, "<redacted>")
	if len(s) > 800 {
		s = s[:800] + "..."
	}
	return s
}

func (c *apiClient) do(ctx context.Context, method, path string, body, out any, ok ...int) (int, error) {
	return c.doAuth(ctx, method, path, c.token, body, out, ok...)
}

func (c *apiClient) doAuth(ctx context.Context, method, path, bearer string, body, out any, ok ...int) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if len(ok) == 0 {
		ok = []int{http.StatusOK}
	}
	accepted := false
	for _, s := range ok {
		accepted = accepted || s == res.StatusCode
	}
	if !accepted {
		return res.StatusCode, &httpError{Method: method, Path: path, Status: res.StatusCode, Body: redact(string(raw))}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return res.StatusCode, fmt.Errorf("%s %s: decode: %w", method, path, err)
		}
	}
	return res.StatusCode, nil
}

// signIn trades the login key for a console session cookie.
func (c *apiClient) signIn(ctx context.Context, key string) error {
	raw, _ := json.Marshal(map[string]string{"api_key": key})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/admin/session", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Weave-Admin", "1")
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return &httpError{Method: "POST", Path: "/v1/admin/session", Status: res.StatusCode, Body: redact(string(body))}
	}
	for _, cookie := range res.Cookies() {
		if cookie.Name == "weave_admin_session" && cookie.Value != "" {
			c.token = cookie.Value
			return nil
		}
	}
	return fmt.Errorf("sign-in returned no session cookie")
}
