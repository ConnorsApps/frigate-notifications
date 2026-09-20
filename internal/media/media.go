// Package media serves Frigate snapshots and clips to phones that are off the
// home network.
//
// Frigate is split-horizon: its in-cluster listener is unauthenticated, its
// public one requires a session a push notification can't carry. So links in
// notifications point here instead, and this proxy fetches from the in-cluster
// side on the phone's behalf. Links are signed and expiring rather than
// token-backed: a signature needs no lookup, so an outstanding notification's
// image still renders when the datastore is down.
package media

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
)

// Kind is the closed set of media a signed link may address. The request path
// is never forwarded verbatim; a kind maps to a fixed upstream path.
type Kind string

const (
	KindSnapshot Kind = "snapshot"
	KindClip     Kind = "clip"
	KindPreview  Kind = "preview"
)

// upstreamPath maps a kind to its Frigate API path. Snapshot takes a Frigate
// *event* id (review.data.detections[0]); preview takes a review id; clip takes
// a ClipID, so it covers the whole review rather than one detection.
func (k Kind) upstreamPath(id string) (string, bool) {
	switch k {
	case KindSnapshot:
		return "/api/events/" + id + "/snapshot.jpg?bbox=1", true
	case KindClip:
		camera, start, end, ok := parseClipID(id)
		if !ok {
			return "", false
		}
		return fmt.Sprintf("/api/%s/start/%d/end/%d/clip.mp4", camera, start, end), true
	case KindPreview:
		// Frigate 0.18: the ".gif" suffix 404s; the bare path serves the gif.
		return "/api/review/" + id + "/preview", true
	}
	return "", false
}

var kindFiles = map[Kind]struct{ ext, contentType string }{
	KindSnapshot: {"jpg", "image/jpeg"},
	KindClip:     {"mp4", "video/mp4"},
	KindPreview:  {"gif", "image/gif"},
}

// ext is the file extension public links carry. iOS infers an attachment's
// type from it, and needs an explicit content-type when there is none.
func (k Kind) ext() string { return kindFiles[k].ext }

func (k Kind) contentType() string { return kindFiles[k].contentType }

// Clip padding around the review, so the clip opens before the person is in
// frame and doesn't cut off as they leave.
const (
	clipPadBefore = 2
	clipPadAfter  = 3
	maxClipSpan   = 2 * 60 * 60
)

// ClipID encodes a review's camera and time span, "<start>-<end>-<camera>".
// Start and end are whole epoch seconds, so they never contain "-" and the
// camera (which may) is everything after the second one.
func ClipID(camera string, start, end float64) string {
	return fmt.Sprintf("%d-%d-%s", int64(start)-clipPadBefore, int64(end)+clipPadAfter, camera)
}

func parseClipID(id string) (camera string, start, end int64, ok bool) {
	parts := strings.SplitN(id, "-", 3)
	if len(parts) != 3 || parts[2] == "" || parts[2] == "." {
		return "", 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	end, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || start <= 0 || end <= start || end-start > maxClipSpan {
		return "", 0, 0, false
	}
	return parts[2], start, end, true
}

// idRe bounds ids to Frigate's charset so nothing can escape the fixed paths
// above. Dot segments are rejected separately by validID: the charset alone
// would admit "..", which the upstream path would then interpolate verbatim.
var idRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func validID(id string) bool {
	return idRe.MatchString(id) && id != "." && id != ".." && !strings.Contains(id, "..")
}

var (
	ErrBadSignature = errors.New("media: bad signature")
	ErrExpired      = errors.New("media: link expired")
	ErrBadRequest   = errors.New("media: malformed link")
)

// Signer mints and verifies media links.
type Signer struct {
	key     []byte
	baseURL string
	ttl     time.Duration
	now     func() time.Time
}

// NewSigner takes a hex-encoded key. Rotating the key invalidates every
// outstanding link, which is the only revocation this design offers — and the
// only one with a realistic trigger.
func NewSigner(hexKey, publicBaseURL string, ttl time.Duration) (*Signer, error) {
	key, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil {
		return nil, fmt.Errorf("media: signing key must be hex: %w", err)
	}
	if len(key) < 16 {
		return nil, fmt.Errorf("media: signing key must be at least 16 bytes (32 hex chars), got %d", len(key))
	}
	if publicBaseURL == "" {
		return nil, errors.New("media: publicBaseURL is required")
	}
	return &Signer{
		key:     key,
		baseURL: strings.TrimSuffix(publicBaseURL, "/"),
		ttl:     ttl,
		now:     time.Now,
	}, nil
}

// SignerFor builds the signer from config, or nil when media is disabled.
func SignerFor(c config.MediaConfig) (*Signer, error) {
	if !c.Enabled() {
		return nil, nil
	}
	return NewSigner(c.SigningKey, c.PublicBaseURL, time.Duration(c.LinkTTL))
}

func (s *Signer) sign(kind Kind, id string, exp int64) string {
	mac := hmac.New(sha256.New, s.key)
	fmt.Fprintf(mac, "%s/%s/%d", kind, id, exp)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// URL returns a signed, expiring link for the given media.
func (s *Signer) URL(kind Kind, id string) (string, error) {
	if _, ok := kind.upstreamPath(id); !ok {
		return "", fmt.Errorf("media: unknown kind %q or malformed id %q", kind, id)
	}
	if !validID(id) {
		return "", fmt.Errorf("media: invalid id %q", id)
	}
	exp := s.now().Add(s.ttl).Unix()
	q := url.Values{
		"exp": {strconv.FormatInt(exp, 10)},
		"sig": {s.sign(kind, id, exp)},
	}
	// The extension is cosmetic, for the phone's benefit: it isn't signed, and
	// serve strips it before Verify.
	return fmt.Sprintf("%s/m/%s/%s.%s?%s", s.baseURL, kind, id, kind.ext(), q.Encode()), nil
}

// Verify checks a link's signature and expiry, returning the time remaining
// on it so the response can be cached for exactly that long.
func (s *Signer) Verify(kind Kind, id, expRaw, sig string) (time.Duration, error) {
	if !validID(id) {
		return 0, ErrBadRequest
	}
	if _, ok := kind.upstreamPath(id); !ok {
		return 0, ErrBadRequest
	}
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil {
		return 0, ErrBadRequest
	}

	// Compare before checking expiry: an unsigned request should never learn
	// anything from the difference between "expired" and "forged".
	want := s.sign(kind, id, exp)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return 0, ErrBadSignature
	}

	remaining := time.Unix(exp, 0).Sub(s.now())
	if remaining <= 0 {
		return 0, ErrExpired
	}
	return remaining, nil
}
