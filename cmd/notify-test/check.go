package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/media"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
)

// ntfyAutoDownload is ntfy Android's default auto-download cap.
const ntfyAutoDownload int64 = 1 << 20

// check fetches every link in m from the public base URL, as a phone or a
// chat service would, and reports what each returned. It returns the exit
// status: 1 if any link failed.
func check(ctx context.Context, m sender.Message) int {
	client := &http.Client{Timeout: 60 * time.Second}
	failed := false
	report := func(what, link string, limit int64, note string) []byte {
		if link == "" {
			fmt.Printf("%-9s (none)\n", what)
			return nil
		}
		r := fetch(ctx, client, link)
		ok := r.err == nil && r.status == http.StatusOK
		failed = failed || !ok
		line := fmt.Sprintf("%-9s %s", what, r)
		if ok && limit > 0 && r.size > limit {
			line += fmt.Sprintf("  over %s", humanBytes(limit))
			if note != "" {
				line += " (" + note + ")"
			}
		}
		if codec := videoCodec(r.head); codec != "" {
			line += "  codec " + codec
			if codec == "hev1" {
				line += ": iOS plays HEVC only tagged hvc1; set the camera's ffmpeg.apple_compatibility: true in Frigate"
			}
		}
		fmt.Println(line)
		fmt.Printf("          %s\n", link)
		return r.body
	}

	report("still", m.Image, media.MaxImageBytes, "iOS won't attach it")
	report("snapshot", m.Snapshot, ntfyAutoDownload, "ntfy on Android won't auto-download it by default")
	report("clip", m.Video, media.MaxClipBytes, "past the budget iOS can download in time")

	page := report("player", m.ClipURL, 0, "")
	if hls := hlsSource(page, m.ClipURL); hls != "" {
		master := report("hls", hls, 0, "")
		if index := resolve(hls, firstURI(master)); index != "" {
			playlist := report("hls index", index, 0, "")
			report("hls init", resolve(index, initURI(playlist)), 0, "")
			report("hls seg", resolve(index, firstURI(playlist)), 0, "")
		}
	}

	if failed {
		return 1
	}
	return 0
}

// fetched is one link's outcome. body is kept for pages and playlists only;
// head is the start of anything else, enough to find a video codec.
type fetched struct {
	status      int
	contentType string
	size        int64
	head, body  []byte
	err         error
}

func (f fetched) String() string {
	if f.err != nil {
		return "error: " + f.err.Error()
	}
	return fmt.Sprintf("%d %s %s", f.status, f.contentType, humanBytes(f.size))
}

func fetch(ctx context.Context, client *http.Client, link string) fetched {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return fetched{err: err}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fetched{err: err}
	}
	defer resp.Body.Close()

	f := fetched{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type")}
	const headSize = 64 << 10
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(resp.Body, headSize))
	if err == nil {
		// The rest is only counted: clips are streamed with no length.
		var rest int64
		rest, err = io.Copy(io.Discard, resp.Body)
		n += rest
	}
	f.size, f.head, f.err = n, buf.Bytes(), err
	if strings.HasPrefix(f.contentType, "text/html") || strings.Contains(f.contentType, "mpegurl") {
		f.body = f.head
	}
	return f
}

// videoCodecs are the sample entry types of the video codecs worth naming.
var videoCodecs = map[string]bool{"avc1": true, "avc3": true, "hvc1": true, "hev1": true, "av01": true, "vp09": true}

// videoCodec is the video sample entry type in an mp4's first bytes: each
// stsd box is followed by its version, entry count and first entry's size,
// then that entry's type. Frigate's fragmented mp4 puts its moov first.
func videoCodec(head []byte) string {
	for rest := head; ; {
		i := bytes.Index(rest, []byte("stsd"))
		if i < 0 || len(rest) < i+20 {
			return ""
		}
		if entry := string(rest[i+16 : i+20]); videoCodecs[entry] {
			return entry
		}
		rest = rest[i+4:]
	}
}

var sourceRe = regexp.MustCompile(`<source src="([^"]+)" type="application/vnd.apple.mpegurl"`)

// hlsSource is the HLS link on the player page, absolute.
func hlsSource(page []byte, pageURL string) string {
	match := sourceRe.FindSubmatch(page)
	if match == nil {
		return ""
	}
	return resolve(pageURL, strings.ReplaceAll(string(match[1]), "&amp;", "&"))
}

// firstURI is a playlist's first variant or segment.
func firstURI(playlist []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(playlist))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

var mapRe = regexp.MustCompile(`#EXT-X-MAP:URI="([^"]+)"`)

// initURI is a media playlist's init segment.
func initURI(playlist []byte) string {
	if match := mapRe.FindSubmatch(playlist); match != nil {
		return string(match[1])
	}
	return ""
}

// resolve makes ref absolute against base, as a player does.
func resolve(base, ref string) string {
	if ref == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return b.ResolveReference(r).String()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
