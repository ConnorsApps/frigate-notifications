package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/media"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
)

// mp4Head is the start of an mp4 with an audio track's stsd ahead of the
// video track's, whose entry type is codec.
func mp4Head(codec string) string {
	entry := func(fourcc string) string {
		return "\x00\x00\x00\x10stsd\x00\x00\x00\x00\x00\x00\x00\x01\x00\x00\x00\x64" + fourcc
	}
	return "\x00\x00\x00\x20ftypisom" + entry("mp4a") + entry(codec) + "moof mdat"
}

func TestVideoCodec(t *testing.T) {
	for _, codec := range []string{"avc1", "hvc1", "hev1"} {
		if got := videoCodec([]byte(mp4Head(codec))); got != codec {
			t.Errorf("videoCodec = %q, want %q", got, codec)
		}
	}
	if got := videoCodec([]byte("\xff\xd8 a jpeg with avc1 in it")); got != "" {
		t.Errorf("videoCodec of a jpeg = %q, want none", got)
	}
}

// check follows the "View Clip" page to its HLS files through the real proxy,
// resolving the playlists' relative links the way a player does. That only
// works because the vod signature is in the path.
func TestCheckFollowsThePlayerThroughHLS(t *testing.T) {
	const vod = "/vod/front_porch/start/1756999998/end/1757000015/"
	files := map[string][2]string{ // path: content type, body
		"/api/events/evt1/snapshot.jpg":                             {"image/jpeg", "\xff\xd8jpeg"},
		"/api/front_porch/start/1756999998/end/1757000015/clip.mp4": {"video/mp4", mp4Head("avc1")},
		vod + "master.m3u8":                                         {"application/vnd.apple.mpegurl", "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nindex-v1.m3u8\n"},
		vod + "index-v1.m3u8":                                       {"application/vnd.apple.mpegurl", "#EXTM3U\n#EXT-X-MAP:URI=\"init-v1.mp4\"\n#EXTINF:10,\nseg-1-v1.m4s\n"},
		vod + "init-v1.mp4":                                         {"video/mp4", mp4Head("avc1")},
		vod + "seg-1-v1.m4s":                                        {"video/mp4", "moof"},
	}
	var served sync.Map
	frigate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		served.Store(r.URL.Path, true)
		w.Header().Set("Content-Type", f[0])
		io.WriteString(w, f[1])
	}))
	t.Cleanup(frigate.Close)

	public := httptest.NewUnstartedServer(nil)
	signer, err := media.NewSigner("0123456789abcdef0123456789abcdef", "http://"+public.Listener.Addr().String(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	public.Config.Handler = media.NewProxy(signer, frigate.URL).Handler()
	public.Start()
	t.Cleanup(public.Close)

	sign := func(kind media.Kind, id string) string {
		link, err := signer.URL(kind, id)
		if err != nil {
			t.Fatal(err)
		}
		return link
	}
	clipID := media.ClipID("front_porch", 1757000000.5, 1757000012.9)
	m := sender.Message{
		Image:    sign(media.KindSnapshot, "evt1"),
		Snapshot: sign(media.KindSnapshot, "evt1"),
		Video:    sign(media.KindClip, clipID),
		ClipURL:  sign(media.KindPlay, clipID),
	}

	if status := check(m); status != 0 {
		t.Errorf("check = %d, want every link to answer", status)
	}
	for path := range files {
		if _, ok := served.Load(path); !ok {
			t.Errorf("Frigate never served %s", path)
		}
	}
}
