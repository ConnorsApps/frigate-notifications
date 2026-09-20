package sender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	otel_bootstrap "github.com/ConnorsApps/frigate-notifications/internal/lib/otel"
)

// httpTimeout is the backstop; the notifier bounds each send too.
const httpTimeout = 20 * time.Second

// maxRetryAfter caps the wait before a rate-limited send's one retry.
const maxRetryAfter = 5 * time.Second

// tracedClient emits OTel spans, which record the full URL: use it only for
// URLs with no secret.
func tracedClient() *http.Client {
	c := otel_bootstrap.NewHTTPClient()
	c.Timeout = httpTimeout
	return c
}

// response is a backend's reply, read and bounded.
type response struct {
	status     int
	body       []byte
	retryAfter time.Duration
}

func (r response) ok() bool { return r.status >= 200 && r.status < 300 }

// err is a status error carrying the start of the body.
func (r response) err(prefix string) error {
	const max = 200
	s := strings.TrimSpace(string(r.body))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return fmt.Errorf("%s: status %d: %s", prefix, r.status, s)
}

// doJSON sends body as JSON, with a bearer token if given. Transport errors
// are unwrapped from *url.Error, which holds the URL; for Discord the URL is
// the credential.
func doJSON(ctx context.Context, client *http.Client, method, target, bearer string, body any) (response, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return response{}, fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return response{}, errors.New("build request")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := client.Do(req)
	if err != nil {
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return response{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return response{}, fmt.Errorf("read response: %w", err)
	}
	return response{
		status:     resp.StatusCode,
		body:       raw,
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}, nil
}

// parseRetryAfter reads the seconds form (fractional on Discord).
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// waitRetryAfter sleeps out a rate limit and reports whether to retry: false
// if the wait is too long or ctx ended.
func waitRetryAfter(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = time.Second
	}
	if d > maxRetryAfter {
		return false
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
