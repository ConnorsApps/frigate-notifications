package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// iOS refuses attachments over 10 MB (images) and 50 MB (video), and the
// notification extension that downloads them gets ~30 s, so clips are held to
// a smaller budget than the hard limit to survive a cellular connection.
const (
	MaxImageBytes int64 = 10 << 20
	MaxClipBytes  int64 = 25 << 20
)

// Limit is the largest size worth attaching for a kind.
func (k Kind) Limit() int64 {
	if k == KindClip {
		return MaxClipBytes
	}
	return MaxImageBytes
}

var ErrTooLarge = errors.New("media: exceeds attachment size limit")

// Prober asks Frigate whether a piece of media exists and fits, so the
// notifier can fall back to a lighter kind instead of attaching a link that
// 404s or that iOS would refuse.
type Prober struct{ frigate }

func NewProber(frigateURL string) *Prober {
	return &Prober{newFrigate(frigateURL, 30*time.Second)}
}

// Check reports nil when the media exists, is the right sort of file, and is
// at most limit bytes.
//
// Clips are streamed by Frigate with no Content-Length, so their size is only
// knowable by reading; the read stops one byte past the limit.
func (p *Prober) Check(ctx context.Context, kind Kind, id string, limit int64) error {
	path, ok := kind.upstreamPath(id)
	if !ok {
		return fmt.Errorf("media: cannot probe kind %q id %q", kind, id)
	}

	resp, err := p.fetch(ctx, kind, path, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("media: upstream %s for %s", resp.Status, kind)
	}
	// Frigate answers some failures with a 200 JSON body.
	want := strings.SplitN(kind.contentType(), "/", 2)[0] + "/"
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, want) {
		return fmt.Errorf("media: upstream Content-Type %q for %s", ct, kind)
	}
	if resp.ContentLength > limit {
		return ErrTooLarge
	}

	n, err := io.CopyN(io.Discard, resp.Body, limit+1)
	switch {
	case n > limit:
		return ErrTooLarge
	case err != nil && !errors.Is(err, io.EOF):
		return err
	case n == 0:
		return fmt.Errorf("media: upstream returned an empty %s", kind)
	}
	return nil
}
