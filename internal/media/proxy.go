package media

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
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
		frigate: newFrigate(frigateURL, proxyStreamTimeout),
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
// health checks live on the internal listener and are unreachable from
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
	if ok && Kind(kindRaw) == kindVOD {
		p.serveVOD(w, r, name)
		return
	}
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
		p.rejectLink(w, err)
		return
	}

	if kind == KindPlay {
		p.servePlay(w, r, id, q.Get("exp"), remaining)
		return
	}
	upstream, _ := kind.upstreamPath(id)
	p.forward(w, r, kind, upstream, kind.contentType(), remaining)
}

// vodFileRe admits the playlists and segments nginx-vod names, and nothing
// that could leave the clip's directory.
var vodFileRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}\.(m3u8|m4s|mp4)$`)

var vodContentTypes = map[string]string{
	".m3u8": "application/vnd.apple.mpegurl",
	".m4s":  "video/iso.segment",
	".mp4":  "video/mp4",
}

// serveVOD serves "<clipID>/<exp>/<sig>/<file>", one of a clip's HLS files,
// from Frigate's /vod on the same in-cluster listener as its API.
func (p *Proxy) serveVOD(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || !vodFileRe.MatchString(parts[3]) {
		p.reject(w, "malformed", http.StatusForbidden)
		return
	}
	clipID, expRaw, sig, file := parts[0], parts[1], parts[2], parts[3]

	remaining, err := p.signer.Verify(kindVOD, clipID, expRaw, sig)
	if err != nil {
		p.rejectLink(w, err)
		return
	}
	camera, start, end, _ := parseClipID(clipID)
	upstream := fmt.Sprintf("/vod/%s/start/%d/end/%d/%s", camera, start, end, file)
	p.forward(w, r, kindVOD, upstream, vodContentTypes[path.Ext(file)], remaining)
}

// forward streams one upstream response to the client, cacheable for the time
// left on its link.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, kind Kind, upstream, contentType string, remaining time.Duration) {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-r.Context().Done():
		return
	case <-time.After(10 * time.Second):
		p.reject(w, "busy", http.StatusServiceUnavailable)
		return
	}

	resp, err := p.fetch(r.Context(), upstream, r.Header.Get("Range"))
	if err != nil {
		p.logger.Warn().Err(err).Str("kind", string(kind)).Str("path", upstream).Msg("upstream fetch failed")
		p.reject(w, "upstream_error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	h := w.Header()
	h.Set("Content-Type", cmp.Or(resp.Header.Get("Content-Type"), contentType))
	// Phones range-request mp4; mirror upstream's range headers and status.
	for _, k := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	// Only a real response is cacheable: an error cached for the link's whole
	// lifetime would outlast whatever caused it.
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

// Upstream limits. A client's Timeout also covers reading the body, and a
// proxied clip is read as fast as the phone downloads it, so the proxy's
// overall limit is generous and only a stalled upstream is cut early, by the
// header timeout; time to first byte is where a slow Frigate shows up.
const (
	upstreamHeaderTimeout = 30 * time.Second
	proxyStreamTimeout    = 15 * time.Minute
)

// frigate is the Frigate client shared by the proxy and the Prober so both
// see the same view of Frigate. It never follows redirects.
type frigate struct {
	base   string
	client *http.Client
}

// newFrigate bounds a whole request, body included, by timeout.
func newFrigate(frigateURL string, timeout time.Duration) frigate {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = upstreamHeaderTimeout
	return frigate{
		base: strings.TrimSuffix(frigateURL, "/"),
		client: &http.Client{
			Transport:     transport,
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// fetch performs the upstream request.
func (u frigate) fetch(ctx context.Context, upstream, rangeHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.base+upstream, nil)
	if err != nil {
		return nil, err
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	return u.client.Do(req)
}

func (p *Proxy) reject(w http.ResponseWriter, reason string, code int) {
	p.metrics.MediaRejected(reason)
	http.Error(w, http.StatusText(code), code)
}

// rejectLink answers a link that failed verification.
func (p *Proxy) rejectLink(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrExpired):
		p.reject(w, "expired", http.StatusForbidden)
	case errors.Is(err, ErrBadSignature):
		p.reject(w, "bad_signature", http.StatusForbidden)
	default:
		p.reject(w, "malformed", http.StatusForbidden)
	}
}
