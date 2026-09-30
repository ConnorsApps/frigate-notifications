package main

import (
	"bufio"
	"bytes"
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
func check(m sender.Message) int {
	client := &http.Client{Timeout: 60 * time.Second}
	failed := false
	report := func(what, link string, limit int64, note string) []byte {
		if link == "" {
			fmt.Printf("%-9s (none)\n", what)
			return nil
		}
		r := fetch(client, link)
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
		return r.head
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

// fetched is one link's outcome. head is its first 64 KB: all of a page or
// playlist, and enough of an mp4 to find its codec.
type fetched struct {
	status      int
	contentType string
	size        int64
	head        []byte
	err         error
}

func (f fetched) String() string {
	if f.err != nil {
		return "error: " + f.err.Error()
	}
	return fmt.Sprintf("%d %s %s", f.status, f.contentType, humanBytes(f.size))
}

func fetch(client *http.Client, link string) fetched {
	resp, err := client.Get(link)
	if err != nil {
		return fetched{err: err}
	}
	defer resp.Body.Close()

	f := fetched{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type")}
	f.head, f.err = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if f.err == nil {
		// The rest is only counted: clips are streamed with no length.
		f.size, f.err = io.Copy(io.Discard, resp.Body)
	}
	f.size += int64(len(f.head))
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
