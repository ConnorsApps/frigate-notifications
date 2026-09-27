package media

import (
	"html/template"
	"net/http"
	"strconv"
	"time"
)

// playPage is what the "View Clip" link opens. Safari can't reliably play the
// mp4 Frigate streams (fragmented, no length, no byte ranges), so the page
// offers Frigate's HLS first, which iOS and Android play natively, and falls
// back to the mp4 where HLS isn't supported (desktop Chrome and Firefox).
var playPage = template.Must(template.New("play").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Camera}} clip</title>
<style>
html, body { height: 100%; margin: 0; background: #000; color: #ccc; font: 15px system-ui, sans-serif; }
body { display: flex; flex-direction: column; }
video { flex: 1; min-height: 0; width: 100%; }
a { color: inherit; padding: 12px 16px; }
</style>
<video controls playsinline preload="metadata">
<source src="{{.HLS}}" type="application/vnd.apple.mpegurl">
<source src="{{.MP4}}" type="video/mp4">
</video>
<a href="{{.MP4}}" download>Download clip</a>
`))

// playCSP allows the page nothing but its own media and inline style.
const playCSP = "default-src 'none'; media-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// servePlay renders the player for a verified play link. Its media links
// expire with it, so the page never outlives what it plays.
func (p *Proxy) servePlay(w http.ResponseWriter, r *http.Request, clipID, expRaw string, remaining time.Duration) {
	exp, _ := strconv.ParseInt(expRaw, 10, 64) // Verify has parsed it
	camera, _, _, _ := parseClipID(clipID)

	h := w.Header()
	h.Set("Content-Type", KindPlay.contentType())
	h.Set("Content-Security-Policy", playCSP)
	// The page's URL carries its signature.
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, max-age="+strconv.Itoa(int(remaining.Seconds())))

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	err := playPage.Execute(w, struct{ Camera, HLS, MP4 string }{
		Camera: camera,
		HLS:    p.signer.vodBase(clipID, exp) + "master.m3u8",
		MP4:    p.signer.link(KindClip, clipID, exp),
	})
	if err != nil {
		p.logger.Debug().Err(err).Msg("client disconnected mid-page")
		return
	}
	p.metrics.MediaServed(string(KindPlay))
}
