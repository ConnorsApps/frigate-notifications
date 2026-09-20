package media

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// clipRetryWindow bounds how long a 404 on a clip is retried. Recording
// segments finalize slightly after a review ends, so a clip fetched the
// instant the notification lands can legitimately 404.
const (
	clipRetryWindow   = 8 * time.Second
	clipRetryInterval = 2 * time.Second
)

// Metrics is the observability hook, kept as an interface so the proxy has no
// direct OTel dependency and tests can assert on decisions.
type Metrics interface {
	MediaServed(kind string)
	MediaRejected(reason string)
}

type nopMetrics struct{}

func (nopMetrics) MediaServed(string)   {}
func (nopMetrics) MediaRejected(string) {}

// Proxy serves signed media links by fetching from Frigate in-cluster.
//
// It is internet-facing, so: a bounded number of concurrent upstream fetches
// (a leaked link must not become a lever to hammer Frigate), explicit
// timeouts, and no redirect following.
type Proxy struct {
	frigate
	signer  *Signer
	sem     chan struct{}
	metrics Metrics
	logger  zerolog.Logger
}

type ProxyOption func(*Proxy)

// WithHTTPClient overrides the upstream client (tests use httptest).
func WithHTTPClient(c *http.Client) ProxyOption { return func(p *Proxy) { p.client = c } }

// WithMetrics attaches counters.
func WithMetrics(m Metrics) ProxyOption { return func(p *Proxy) { p.metrics = m } }

func NewProxy(signer *Signer, frigateURL string, opts ...ProxyOption) *Proxy {
	p := &Proxy{
		frigate: newFrigate(frigateURL, 60*time.Second),
		signer:  signer,
		sem:     make(chan struct{}, 8),
		metrics: nopMetrics{},
		logger:  log.With().Str("logger", "media").Logger(),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Handler returns the mux for the public listener. Only /m/ is routed here;
// health and metrics live on the internal listener and are unreachable from
// the public gateway.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/m/", http.StripPrefix("/m/", http.HandlerFunc(p.serve)))
	return mux
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	kindRaw, name, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || strings.Contains(name, "/") {
		p.reject(w, "malformed", http.StatusNotFound)
		return
	}
	kind := Kind(kindRaw)
	id, ok := strings.CutSuffix(name, "."+kind.ext())
	if kind.ext() == "" || !ok {
		p.reject(w, "malformed", http.StatusForbidden)
		return
	}

	q := r.URL.Query()
	remaining, err := p.signer.Verify(kind, id, q.Get("exp"), q.Get("sig"))
	if err != nil {
		switch {
		case errors.Is(err, ErrExpired):
			p.reject(w, "expired", http.StatusForbidden)
		case errors.Is(err, ErrBadSignature):
			p.reject(w, "bad_signature", http.StatusForbidden)
		default:
			p.reject(w, "malformed", http.StatusForbidden)
		}
		return
	}

	path, _ := kind.upstreamPath(id)

	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-r.Context().Done():
		return
	case <-time.After(10 * time.Second):
		p.reject(w, "busy", http.StatusServiceUnavailable)
		return
	}

	resp, err := p.fetch(r.Context(), kind, path, r.Header.Get("Range"))
	if err != nil {
		p.logger.Warn().Err(err).Str("kind", string(kind)).Str("id", id).Msg("upstream fetch failed")
		p.reject(w, "upstream_error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	h := w.Header()
	h.Set("Content-Type", cmp.Or(resp.Header.Get("Content-Type"), kind.contentType()))
	// Phones range-request mp4; mirror upstream's range headers and status.
	for _, k := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	// Only a real response is cacheable. A clip fetched before its recording
	// segments finalize legitimately 404s — the retry above exists for that —
	// and caching that 404 for the link's whole lifetime would leave the phone
	// showing nothing long after the clip exists.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.Set("Cache-Control", "private, max-age="+strconv.Itoa(int(remaining.Seconds())))
	} else {
		h.Set("Cache-Control", "no-store")
	}
	h.Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		p.logger.Debug().Err(err).Msg("client disconnected mid-stream")
		return
	}
	p.metrics.MediaServed(string(kind))
}

// frigate is the Frigate client shared by the proxy and the Prober so both
// see the same view of Frigate. It never follows redirects.
type frigate struct {
	base   string
	client *http.Client
}

func newFrigate(frigateURL string, timeout time.Duration) frigate {
	return frigate{
		base: strings.TrimSuffix(frigateURL, "/"),
		client: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// fetch performs the upstream request, retrying a 404 on clips while the
// recording segments finalize.
func (u frigate) fetch(ctx context.Context, kind Kind, path, rangeHeader string) (*http.Response, error) {
	deadline := time.Now()
	if kind == KindClip {
		deadline = deadline.Add(clipRetryWindow)
	}

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.base+path, nil)
		if err != nil {
			return nil, err
		}
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}

		resp, err := u.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusNotFound || !time.Now().Before(deadline) {
			return resp, nil
		}
		resp.Body.Close()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(clipRetryInterval):
		}
	}
}

func (p *Proxy) reject(w http.ResponseWriter, reason string, code int) {
	p.metrics.MediaRejected(reason)
	http.Error(w, http.StatusText(code), code)
}
