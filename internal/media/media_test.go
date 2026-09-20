package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "0123456789abcdef0123456789abcdef"

func newSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(testKey, "https://media.example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewSignerRejectsWeakKeys(t *testing.T) {
	if _, err := NewSigner("nothex!!", "https://x", time.Hour); err == nil {
		t.Error("a non-hex key should be rejected")
	}
	if _, err := NewSigner("abcd", "https://x", time.Hour); err == nil {
		t.Error("a short key should be rejected")
	}
	if _, err := NewSigner(testKey, "", time.Hour); err == nil {
		t.Error("an empty public base URL should be rejected")
	}
}

// parseLink pulls the pieces back out of a signed URL.
func parseLink(t *testing.T, raw string) (Kind, string, string, string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/m/"), "/")
	if len(parts) != 2 {
		t.Fatalf("unexpected path %q", u.Path)
	}
	kind := Kind(parts[0])
	id, ok := strings.CutSuffix(parts[1], "."+kind.ext())
	if !ok {
		t.Fatalf("path %q has no .%s extension", u.Path, kind.ext())
	}
	return kind, id, u.Query().Get("exp"), u.Query().Get("sig")
}

// testID is a valid id for each kind.
func testID(kind Kind) string {
	if kind == KindClip {
		return ClipID("front_porch", 1757000000.5, 1757000012.9)
	}
	return "1757000000.123456-abc123"
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	s := newSigner(t)
	for _, kind := range []Kind{KindSnapshot, KindClip, KindPreview} {
		link, err := s.URL(kind, testID(kind))
		if err != nil {
			t.Fatalf("URL(%s): %v", kind, err)
		}
		gotKind, id, exp, sig := parseLink(t, link)
		remaining, err := s.Verify(gotKind, id, exp, sig)
		if err != nil {
			t.Errorf("Verify(%s): %v", kind, err)
		}
		if remaining <= 0 || remaining > time.Hour {
			t.Errorf("remaining = %s, want (0, 1h]", remaining)
		}
	}
}

func TestVerifyRejectsTamperedLinks(t *testing.T) {
	s := newSigner(t)
	link, err := s.URL(KindSnapshot, "evt1")
	if err != nil {
		t.Fatal(err)
	}
	kind, id, exp, sig := parseLink(t, link)

	tests := []struct {
		name         string
		kind         Kind
		id, exp, sig string
		want         error
	}{
		{name: "swapped kind", kind: KindPreview, id: id, exp: exp, sig: sig, want: ErrBadSignature},
		{name: "swapped id", kind: kind, id: "evt2", exp: exp, sig: sig, want: ErrBadSignature},
		{name: "extended expiry", kind: kind, id: id, exp: "99999999999", sig: sig, want: ErrBadSignature},
		{name: "forged signature", kind: kind, id: id, exp: exp, sig: "AAAA", want: ErrBadSignature},
		{name: "missing signature", kind: kind, id: id, exp: exp, sig: "", want: ErrBadSignature},
		{name: "non-numeric expiry", kind: kind, id: id, exp: "soon", sig: sig, want: ErrBadRequest},
		{name: "path traversal in id", kind: kind, id: "../../config", exp: exp, sig: sig, want: ErrBadRequest},
		{name: "unknown kind", kind: "config", id: id, exp: exp, sig: sig, want: ErrBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Verify(tc.kind, tc.id, tc.exp, tc.sig); err != tc.want {
				t.Errorf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyRejectsExpiredLinks(t *testing.T) {
	s := newSigner(t)
	// Mint a link an hour in the past.
	s.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	link, err := s.URL(KindSnapshot, "evt1")
	if err != nil {
		t.Fatal(err)
	}
	s.now = time.Now

	kind, id, exp, sig := parseLink(t, link)
	if _, err := s.Verify(kind, id, exp, sig); err != ErrExpired {
		t.Errorf("Verify = %v, want ErrExpired", err)
	}
}

// upstream stands in for Frigate.
type upstream struct {
	*httptest.Server
	requests atomic.Int32
	ranges   atomic.Value // last Range header
	notFound atomic.Int32 // 404 this many times before succeeding
}

func newUpstream(t *testing.T, body string) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.requests.Add(1)
		u.ranges.Store(r.Header.Get("Range"))

		if u.notFound.Load() > 0 {
			u.notFound.Add(-1)
			http.NotFound(w, r)
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			w.Header().Set("Content-Range", "bytes 0-3/"+strconv.Itoa(len(body)))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, body[:4])
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func TestProxyServesSignedLinks(t *testing.T) {
	up := newUpstream(t, "jpegbytes")
	s := newSigner(t)
	proxy := httptest.NewServer(NewProxy(s, up.URL, WithHTTPClient(up.Client())).Handler())
	t.Cleanup(proxy.Close)

	link, err := s.URL(KindSnapshot, "evt1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(strings.Replace(link, "https://media.example.com", proxy.URL, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "jpegbytes" {
		t.Errorf("body = %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.HasPrefix(cc, "private,") {
		t.Errorf("Cache-Control = %q, want it to be private", cc)
	}
}

// Phones range-request mp4, so the proxy must pass the header up and mirror
// the 206 back down. Collapsing this to a 200 breaks video playback.
func TestProxyForwardsRangeRequests(t *testing.T) {
	up := newUpstream(t, "mp4bytesmp4bytes")
	s := newSigner(t)
	proxy := httptest.NewServer(NewProxy(s, up.URL, WithHTTPClient(up.Client())).Handler())
	t.Cleanup(proxy.Close)

	link, err := s.URL(KindClip, testID(KindClip))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, strings.Replace(link, "https://media.example.com", proxy.URL, 1), nil)
	req.Header.Set("Range", "bytes=0-3")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}
	if got := up.ranges.Load(); got != "bytes=0-3" {
		t.Errorf("upstream saw Range %q, want it forwarded", got)
	}
	if cr := resp.Header.Get("Content-Range"); cr == "" {
		t.Error("Content-Range should be mirrored back to the client")
	}
}

func TestProxyRejectsUnsignedRequests(t *testing.T) {
	up := newUpstream(t, "x")
	s := newSigner(t)
	proxy := httptest.NewServer(NewProxy(s, up.URL, WithHTTPClient(up.Client())).Handler())
	t.Cleanup(proxy.Close)

	for _, path := range []string{
		"/m/snapshot/evt1",
		"/m/snapshot/evt1?exp=99999999999&sig=forged",
		"/m/config/evt1?exp=99999999999&sig=forged",
	} {
		resp, err := http.Get(proxy.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", path, resp.StatusCode)
		}
	}
	if up.requests.Load() != 0 {
		t.Error("an unsigned request must never reach Frigate")
	}
}

// A clip requested the instant a review ends can legitimately 404 while
// Frigate finishes writing its recording segments.
func TestProxyRetriesClip404(t *testing.T) {
	up := newUpstream(t, "mp4bytes")
	up.notFound.Store(1)

	s := newSigner(t)
	proxy := httptest.NewServer(NewProxy(s, up.URL, WithHTTPClient(up.Client())).Handler())
	t.Cleanup(proxy.Close)

	link, _ := s.URL(KindClip, testID(KindClip))
	resp, err := http.Get(strings.Replace(link, "https://media.example.com", proxy.URL, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want the retry to succeed with 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.HasPrefix(got, "private, max-age=") {
		t.Errorf("Cache-Control = %q, want a private max-age on a real response", got)
	}
	if up.requests.Load() < 2 {
		t.Errorf("upstream saw %d requests, want a retry", up.requests.Load())
	}
}

// Snapshots are not retried: they exist immediately, so a 404 is real and
// retrying only delays the response.
func TestProxyDoesNotRetrySnapshot404(t *testing.T) {
	up := newUpstream(t, "jpeg")
	up.notFound.Store(1)

	s := newSigner(t)
	proxy := httptest.NewServer(NewProxy(s, up.URL, WithHTTPClient(up.Client())).Handler())
	t.Cleanup(proxy.Close)

	link, _ := s.URL(KindSnapshot, "evt1")
	resp, err := http.Get(strings.Replace(link, "https://media.example.com", proxy.URL, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 passed straight through", resp.StatusCode)
	}
	if up.requests.Load() != 1 {
		t.Errorf("upstream saw %d requests, want exactly 1", up.requests.Load())
	}
	// A 404 must never be cached for the link's lifetime: a clip that isn't
	// written yet would then stay missing on the phone long after it exists.
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q on a 404, want no-store", got)
	}
}

// The id is interpolated into a fixed upstream path, so a dot segment must be
// refused even though the charset would admit it. Reachable only by someone
// who can already sign links, which is exactly why it should cost nothing.
func TestSignerRejectsDotSegmentIDs(t *testing.T) {
	s := newSigner(t)
	for _, id := range []string{"..", ".", "../events", "ev..t"} {
		if _, err := s.URL(KindSnapshot, id); err == nil {
			t.Errorf("URL(%q) was signed, want an error", id)
		}
		if _, err := s.Verify(KindSnapshot, id, "1", "sig"); err != ErrBadRequest {
			t.Errorf("Verify(%q) = %v, want ErrBadRequest", id, err)
		}
	}
}

func TestLinksCarryAFileExtension(t *testing.T) {
	s := newSigner(t)
	for kind, ext := range map[Kind]string{KindSnapshot: ".jpg", KindClip: ".mp4", KindPreview: ".gif"} {
		link, err := s.URL(kind, testID(kind))
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(link)
		if !strings.HasSuffix(u.Path, ext) {
			t.Errorf("%s link path %q, want suffix %s", kind, u.Path, ext)
		}
	}
}

func TestProxyRejectsMissingOrWrongExtension(t *testing.T) {
	up := newUpstream(t, "x")
	s := newSigner(t)
	proxy := httptest.NewServer(NewProxy(s, up.URL, WithHTTPClient(up.Client())).Handler())
	t.Cleanup(proxy.Close)

	link, _ := s.URL(KindSnapshot, "evt1")
	for _, suffix := range []string{".mp4", ""} {
		bad := strings.Replace(link, ".jpg?", suffix+"?", 1)
		resp, err := http.Get(strings.Replace(bad, "https://media.example.com", proxy.URL, 1))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("suffix %q: status = %d, want 403", suffix, resp.StatusCode)
		}
	}
	if up.requests.Load() != 0 {
		t.Error("a malformed link must never reach Frigate")
	}
}

func TestUpstreamPaths(t *testing.T) {
	tests := []struct {
		kind Kind
		id   string
		want string
	}{
		{KindSnapshot, "1.2-abc", "/api/events/1.2-abc/snapshot.jpg?bbox=1"},
		{KindPreview, "1.2-abc", "/api/review/1.2-abc/preview"},
		{KindClip, ClipID("front_porch", 1789839974.046, 1789839979.9), "/api/front_porch/start/1789839972/end/1789839982/clip.mp4"},
		{KindClip, "5-10-cam-with-dashes", "/api/cam-with-dashes/start/5/end/10/clip.mp4"},
	}
	for _, tc := range tests {
		got, ok := tc.kind.upstreamPath(tc.id)
		if !ok || got != tc.want {
			t.Errorf("%s %q = %q (ok=%v), want %q", tc.kind, tc.id, got, ok, tc.want)
		}
	}

	for _, id := range []string{"evt1", "10-5-cam", "1-99999-cam", "abc-10-cam", "5-10-", "5-10-.", "0-10-cam"} {
		if _, ok := KindClip.upstreamPath(id); ok {
			t.Errorf("clip id %q should be rejected", id)
		}
	}
}

func TestProberCheck(t *testing.T) {
	serve := func(status int, ctype, body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ctype)
			w.WriteHeader(status)
			io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	tests := []struct {
		name    string
		status  int
		ctype   string
		body    string
		limit   int64
		wantErr string
	}{
		{name: "fits", status: 200, ctype: "image/jpeg", body: "12345", limit: 10},
		{name: "exactly at the limit", status: 200, ctype: "image/jpeg", body: "12345", limit: 5},
		{name: "one byte over", status: 200, ctype: "image/jpeg", body: "123456", limit: 5, wantErr: "size limit"},
		{name: "404", status: 404, ctype: "application/json", body: `{}`, limit: 10, wantErr: "404"},
		{name: "200 json is not an image", status: 200, ctype: "application/json", body: `{"e":1}`, limit: 10, wantErr: "Content-Type"},
		{name: "empty body", status: 200, ctype: "image/jpeg", body: "", limit: 10, wantErr: "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(tc.status, tc.ctype, tc.body)
			err := NewProber(srv.URL).Check(context.Background(), KindSnapshot, "evt1", tc.limit)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Check = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Check = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// Frigate streams clips chunked with no Content-Length, so the prober has to
// count bytes rather than trust a header.
func TestProberCountsStreamedClips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.(http.Flusher).Flush() // forces chunked, no Content-Length
		io.WriteString(w, strings.Repeat("x", 100))
	}))
	t.Cleanup(srv.Close)

	p := NewProber(srv.URL)
	id := testID(KindClip)
	if err := p.Check(context.Background(), KindClip, id, 100); err != nil {
		t.Errorf("100 bytes under a 100 byte limit: %v", err)
	}
	if err := p.Check(context.Background(), KindClip, id, 99); !errors.Is(err, ErrTooLarge) {
		t.Errorf("100 bytes under a 99 byte limit = %v, want ErrTooLarge", err)
	}
}
