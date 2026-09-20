package sender

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
)

// recorded is one request a test server saw.
type recorded struct {
	method string
	path   string
	query  string
	auth   string
	body   []byte
}

func (r recorded) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, r.body)
	}
	return out
}

// server records requests and answers from script, repeating the last reply.
type server struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

type reply struct {
	status int
	body   string
	header map[string]string
}

func newServer(t *testing.T, script ...reply) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		s.mu.Lock()
		n := len(s.reqs)
		s.reqs = append(s.reqs, recorded{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), body: body,
		})
		s.mu.Unlock()

		rep := reply{status: 200, body: `{}`}
		if len(script) > 0 {
			rep = script[min(n, len(script)-1)]
		}
		for k, v := range rep.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(rep.status)
		io.WriteString(w, rep.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) all() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.reqs...)
}

func (s *server) only(t *testing.T) recorded {
	t.Helper()
	reqs := s.all()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	return reqs[0]
}

// A real review id, which has a "." that some backends refuse in a key.
const reviewTag = "fn-1787614651.122803-w0zlfu"

var reviewStart = time.Date(2026, 9, 19, 21, 37, 0, 0, time.UTC)

// started is a first alert: a snapshot, no clip yet.
func started() Message {
	return Message{
		Camera:   "front_porch",
		Title:    "Front Porch",
		Headline: "Person detected in Driveway",
		Body:     "Person detected in Driveway",
		Objects:  []string{"person"},
		Zones:    []string{"Driveway"},
		Severity: "alert",
		Stage:    StageStarted,
		Start:    reviewStart,
		Tag:      reviewTag,
		Image:    "https://media.test/m/snapshot/e1.jpg?exp=1&sig=abc",
		ClickURL: "https://ha.test/lovelace/frigate",
	}
}

// ended is the update that replaces the alert: clip ready, GenAI text.
func ended() Message {
	m := started()
	m.Stage = StageEnded
	m.Update = true
	m.End = reviewStart.Add(42 * time.Second)
	m.Detail = "Walking to the door."
	m.Body = m.Headline + "\n" + m.Detail
	m.Image = "https://media.test/m/preview/r1.gif?exp=1&sig=abc"
	m.Video = "https://media.test/m/clip/c1.mp4?exp=1&sig=abc"
	m.ClipURL = m.Video
	return m
}

// message is the full end-phase notification most tests start from.
var message = ended()

// digest is the summary of overflow past a recipient's hourly cap.
func digest() Message {
	return Message{
		Camera:   "front_porch",
		Title:    "Front Porch",
		Headline: "3 more events since 21:37",
		Body:     "3 more events since 21:37",
		Tag:      "fn-digest-front_porch",
		Stage:    StageDigest,
		ClickURL: "https://ha.test/lovelace/frigate",
	}
}

// ---- Home Assistant ----

type fakeHassClient struct {
	service string
	payload map[string]any
	err     error
}

func (f *fakeHassClient) Notify(_ context.Context, service string, data map[string]any) error {
	f.service, f.payload = service, data
	return f.err
}

func hassData(t *testing.T, m Message, dashboard string) map[string]any {
	t.Helper()
	client := &fakeHassClient{}
	if _, err := NewHass(client, dashboard).Send(context.Background(), config.Target{Type: config.TargetHass, Service: "s"}, m, ""); err != nil {
		t.Fatal(err)
	}
	return client.payload["data"].(map[string]any)
}

func TestHassSends(t *testing.T) {
	client := &fakeHassClient{}
	h := NewHass(client, "/lovelace/frigate")

	m := started()
	m.LiveEntity = "camera.front"
	ref, err := h.Send(context.Background(), config.Target{Type: config.TargetHass, Service: "mobile_app_x"}, m, "")
	if err != nil || ref != "" {
		t.Fatalf("Send = %q, %v; want no ref and no error", ref, err)
	}
	if client.service != "mobile_app_x" || client.payload["title"] != "Front Porch" || client.payload["message"] != m.Body {
		t.Errorf("service/title/message = %q / %v / %v", client.service, client.payload["title"], client.payload["message"])
	}

	data := client.payload["data"].(map[string]any)
	for key, want := range map[string]any{
		"tag": reviewTag, "group": "frigate-front_porch", "subtitle": "Alert · Driveway",
		"when": reviewStart.Unix(), "entity_id": "camera.front", "channel": "Frigate",
		"url": "/lovelace/frigate", "clickAction": "/lovelace/frigate",
		"image": m.Image, "alert_once": true,
	} {
		if data[key] != want {
			t.Errorf("data[%q] = %v, want %v", key, data[key], want)
		}
	}
	if data["push"].(map[string]any)["interruption-level"] != "time-sensitive" {
		t.Errorf("a new alert should get through a Focus mode: %v", data["push"])
	}
	if _, ok := data["actions"]; ok {
		t.Error("no clip yet, so no clip action")
	}
}

// Android takes the still, iOS the clip attachment.
func TestHassGivesEachPlatformItsMedia(t *testing.T) {
	data := hassData(t, ended(), "/lovelace/frigate")

	if data["image"] != message.Image {
		t.Errorf("image = %v, want the still for Android", data["image"])
	}
	att := data["attachment"].(map[string]any)
	if att["url"] != message.Video || att["content-type"] != "video/mp4" {
		t.Errorf("attachment = %v, want the clip for iOS", att)
	}
	if _, ok := data["video"]; ok {
		t.Error("Android must not be given a video when it has a still")
	}
	actions := data["actions"].([]map[string]any)
	if len(actions) != 1 || actions[0]["uri"] != message.ClipURL {
		t.Errorf("actions = %v, want the clip only (the tap already opens the dashboard)", actions)
	}
}

func TestHassMediaFallbacks(t *testing.T) {
	m := ended()
	m.Image = ""
	if data := hassData(t, m, ""); data["video"] != m.Video || data["attachment"] != nil || data["image"] != nil {
		t.Errorf("with no still the clip goes alone as video: %v", data)
	}

	m = ended()
	m.Video = ""
	if data := hassData(t, m, ""); data["image"] != m.Image || data["video"] != nil || data["attachment"] != nil {
		t.Errorf("with no clip that fits the still goes alone: %v", data)
	}
	if _, ok := hassData(t, m, "")["url"]; ok {
		t.Error("no dashboardPath configured, so no tap target")
	}
}

// Updates are quiet, and drop the critical push (iOS can't replace it).
func TestHassUpdatesAreQuiet(t *testing.T) {
	for name, critical := range map[string]bool{"normal": false, "critical": true} {
		t.Run(name, func(t *testing.T) {
			m := ended()
			m.Critical = critical
			data := hassData(t, m, "")

			if data["alert_once"] != true {
				t.Error("Android must stay silent on the replace")
			}
			push := data["push"].(map[string]any)
			if push["interruption-level"] != "passive" || push["sound"] != "none" {
				t.Errorf("push = %v, want a passive, soundless replace", push)
			}
			if data["channel"] != "Frigate" || data["ttl"] != nil || data["color"] != nil {
				t.Errorf("an update should not carry critical styling: %v", data)
			}
		})
	}
}

func TestHassCritical(t *testing.T) {
	m := started()
	m.Critical = true
	data := hassData(t, m, "")

	if data["channel"] != "alarm_stream" || data["ttl"] != 0 || data["priority"] != "high" {
		t.Errorf("Android critical = %v, want the alarm stream, now, high priority", data)
	}
	push := data["push"].(map[string]any)
	if push["interruption-level"] != "critical" {
		t.Errorf("push = %v, want a critical alert", push)
	}
	if push["sound"].(map[string]any)["critical"] != 1 {
		t.Errorf("sound = %v, want the critical sound", push["sound"])
	}
}

// A detection is not an alert: it shouldn't break through a Focus mode.
func TestHassDetectionIsNotTimeSensitive(t *testing.T) {
	m := started()
	m.Severity = "detection"
	if data := hassData(t, m, ""); data["push"] != nil || data["subtitle"] != "Detection · Driveway" {
		t.Errorf("data = %v", data)
	}
}

func TestHassDigest(t *testing.T) {
	data := hassData(t, digest(), "")

	if data["channel"] != "Frigate Digest" || data["importance"] != "low" {
		t.Errorf("channel/importance = %v/%v, want a low-importance digest channel", data["channel"], data["importance"])
	}
	if data["push"].(map[string]any)["interruption-level"] != "passive" {
		t.Errorf("push = %v", data["push"])
	}
	if _, ok := data["subtitle"]; ok {
		t.Error("a digest has no severity or zone to show")
	}
}

func TestHassSendErrorIsReturned(t *testing.T) {
	h := NewHass(&fakeHassClient{err: errors.New("down")}, "")
	if _, err := h.Send(context.Background(), config.Target{Service: "s"}, message, ""); err == nil {
		t.Error("a Home Assistant failure must reach the caller")
	}
}

// ---- ntfy ----

func ntfyBody(t *testing.T, m Message) map[string]any {
	t.Helper()
	srv := newServer(t)
	n := NewNtfy(config.NtfyConfig{URL: srv.URL + "/", Token: "tk_secret"})
	ref, err := n.Send(context.Background(), config.Target{Type: config.TargetNtfy, Topic: "alice"}, m, "")
	if err != nil || ref != "" {
		t.Fatalf("Send = %q, %v; want no ref (ntfy replaces by sequence id)", ref, err)
	}
	req := srv.only(t)
	if req.method != "POST" || req.path != "/" || req.auth != "Bearer tk_secret" {
		t.Errorf("request = %s %s auth %q, want an authenticated POST /", req.method, req.path, req.auth)
	}
	return req.json(t)
}

func tagsOf(body map[string]any) []string {
	var tags []string
	for _, tag := range body["tags"].([]any) {
		tags = append(tags, tag.(string))
	}
	return tags
}

// Real review ids contain "."; ntfy 400s on it in a sequence id.
func TestNtfySequenceIDIsValidForARealReviewID(t *testing.T) {
	body := ntfyBody(t, started())
	if got := body["sequence_id"]; got != "fn-1787614651_122803-w0zlfu" {
		t.Errorf("sequence_id = %v", got)
	}
	if body["topic"] != "alice" {
		t.Errorf("topic = %v", body["topic"])
	}

	valid := regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)
	for _, tag := range []string{reviewTag, "fn-digest-front porch", "fn-é/ü", strings.Repeat("x", 100), "fn-1787614651.122803-w0zlfu-esc"} {
		if id := ntfySequenceID(tag); !valid.MatchString(id) {
			t.Errorf("ntfySequenceID(%q) = %q, want one ntfy accepts", tag, id)
		}
	}
}

func TestNtfyNewAlert(t *testing.T) {
	m := started()
	body := ntfyBody(t, m)

	if body["title"] != "Front Porch" || body["message"] != "Person detected in Driveway" {
		t.Errorf("title/message = %v / %v", body["title"], body["message"])
	}
	if body["priority"] != float64(4) || !slices.Equal(tagsOf(body), []string{"walking"}) {
		t.Errorf("priority/tags = %v / %v, want a high-priority walking person", body["priority"], body["tags"])
	}
	if body["attach"] != m.Image || body["click"] != m.ClickURL {
		t.Errorf("attach/click = %v / %v", body["attach"], body["click"])
	}
	if _, ok := body["actions"]; ok {
		t.Error("no clip yet, so no action: the tap already opens the dashboard")
	}
}

func TestNtfyDetectionCriticalAndThreat(t *testing.T) {
	m := started()
	m.Severity = "detection"
	if body := ntfyBody(t, m); body["priority"] != float64(3) {
		t.Errorf("detection priority = %v, want the default", body["priority"])
	}

	m = started()
	m.Critical = true
	body := ntfyBody(t, m)
	if body["priority"] != float64(5) || !slices.Equal(tagsOf(body), []string{"rotating_light", "walking"}) {
		t.Errorf("critical priority/tags = %v / %v", body["priority"], body["tags"])
	}

	m = ended()
	m.Threat = 2
	if tags := tagsOf(ntfyBody(t, m)); !slices.Contains(tags, "warning") {
		t.Errorf("tags = %v, want a warning for a critical GenAI verdict", tags)
	}

	m = started()
	m.Objects = []string{"bicycle"}
	if tags := tagsOf(ntfyBody(t, m)); !slices.Equal(tags, []string{"eyes"}) {
		t.Errorf("an unmapped label gets %v, want eyes", tags)
	}
}

// Updates are quiet, critical reviews included.
func TestNtfyUpdateIsQuietAndOffersTheClip(t *testing.T) {
	for name, critical := range map[string]bool{"normal": false, "critical": true} {
		t.Run(name, func(t *testing.T) {
			m := ended()
			m.Critical = critical
			body := ntfyBody(t, m)

			if body["priority"] != float64(2) {
				t.Errorf("priority = %v, want quiet", body["priority"])
			}
			if body["attach"] != m.Image {
				t.Errorf("attach = %v, want the still: clients preview images, not mp4", body["attach"])
			}
			actions := body["actions"].([]any)
			clip := actions[0].(map[string]any)
			if len(actions) != 1 || clip["label"] != "View Clip" || clip["action"] != "view" ||
				clip["url"] != m.ClipURL || clip["clear"] != true {
				t.Errorf("actions = %v", actions)
			}
			if body["message"] != "Person detected in Driveway\nWalking to the door." {
				t.Errorf("message = %q", body["message"])
			}
		})
	}
}

// The header API is ASCII-only; this is why the body is JSON.
func TestNtfyKeepsNonASCIIText(t *testing.T) {
	m := started()
	m.Title, m.Body = "Café", "Élodie detected — at the door"
	body := ntfyBody(t, m)
	if body["title"] != "Café" || body["message"] != "Élodie detected — at the door" {
		t.Errorf("title/message = %q / %q", body["title"], body["message"])
	}
}

func TestNtfyDigest(t *testing.T) {
	body := ntfyBody(t, digest())
	if body["priority"] != float64(2) || !slices.Equal(tagsOf(body), []string{"bar_chart"}) {
		t.Errorf("priority/tags = %v / %v, want a quiet summary", body["priority"], body["tags"])
	}
	if _, ok := body["attach"]; ok {
		t.Error("a digest has no picture")
	}
}

func TestNtfyOmitsWhatItDoesNotHave(t *testing.T) {
	srv := newServer(t)
	n := NewNtfy(config.NtfyConfig{URL: srv.URL})

	m := Message{Title: "T", Body: "B", Tag: "x", Stage: StageStarted}
	if _, err := n.Send(context.Background(), config.Target{Topic: "t"}, m, ""); err != nil {
		t.Fatal(err)
	}
	req := srv.only(t)
	if req.auth != "" {
		t.Errorf("auth = %q, want none without a token", req.auth)
	}
	body := req.json(t)
	for _, k := range []string{"attach", "click", "actions"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s present, want it omitted", k)
		}
	}
}

func TestNtfyRetriesServerErrorsButNotClientErrors(t *testing.T) {
	t.Run("5xx then success", func(t *testing.T) {
		srv := newServer(t, reply{status: 502, body: "bad gateway"}, reply{status: 200, body: `{}`})
		n := NewNtfy(config.NtfyConfig{URL: srv.URL})
		if _, err := n.Send(context.Background(), config.Target{Topic: "t"}, message, ""); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if got := len(srv.all()); got != 2 {
			t.Errorf("requests = %d, want 2", got)
		}
	})

	t.Run("4xx is final", func(t *testing.T) {
		srv := newServer(t, reply{status: 403, body: "forbidden"})
		n := NewNtfy(config.NtfyConfig{URL: srv.URL})
		_, err := n.Send(context.Background(), config.Target{Topic: "t"}, message, "")
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("err = %v, want the 403", err)
		}
		if got := len(srv.all()); got != 1 {
			t.Errorf("requests = %d, want no retry on a client error", got)
		}
	})
}

func TestNtfyVerify(t *testing.T) {
	srv := newServer(t, reply{status: 200, body: `{"healthy":true}`})
	if err := NewNtfy(config.NtfyConfig{URL: srv.URL}).Verify(context.Background(), nil); err != nil {
		t.Errorf("Verify = %v", err)
	}
	if srv.only(t).path != "/v1/health" {
		t.Error("Verify should hit /v1/health")
	}

	down := newServer(t, reply{status: 503})
	if err := NewNtfy(config.NtfyConfig{URL: down.URL}).Verify(context.Background(), nil); err == nil {
		t.Error("an unhealthy server should fail Verify")
	}
}

// ---- Slack ----

func newSlack(srv *server) *Slack {
	s := NewSlack(config.SlackConfig{BotToken: "xoxb-test"})
	s.apiURL = srv.URL
	return s
}

func blockTypes(body map[string]any) []string {
	var types []string
	for _, b := range body["blocks"].([]any) {
		types = append(types, b.(map[string]any)["type"].(string))
	}
	return types
}

func TestSlackPostsAndReturnsTheRepliedChannel(t *testing.T) {
	// A user id posts into a DM, and Slack answers with that DM's D... id.
	srv := newServer(t, reply{status: 200, body: `{"ok":true,"channel":"D999","ts":"1700.0001"}`})

	ref, err := newSlack(srv).Send(context.Background(), config.Target{Type: config.TargetSlack, Channel: "U123"}, message, "")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ref != "D999:1700.0001" {
		t.Errorf("ref = %q, want the DM channel from the reply, not the configured user id", ref)
	}

	req := srv.only(t)
	if req.path != "/chat.postMessage" || req.auth != "Bearer xoxb-test" {
		t.Errorf("request = %s auth %q", req.path, req.auth)
	}
	body := req.json(t)
	if body["channel"] != "U123" {
		t.Errorf("channel = %v", body["channel"])
	}
	if got := strings.Join(blockTypes(body), ","); got != "header,section,image,context" {
		t.Errorf("blocks = %s", got)
	}
	if body["text"] == "" {
		t.Error("fallback text is required alongside blocks")
	}
	if body["unfurl_links"] != false || body["unfurl_media"] != false {
		t.Error("links must not unfurl: the signed media URLs would be fetched again")
	}

	blocks := body["blocks"].([]any)
	image := blocks[2].(map[string]any)
	if image["image_url"] != message.Image || image["alt_text"] == "" {
		t.Errorf("image block = %v, want the still with alt_text", image)
	}
	ctxText := blocks[3].(map[string]any)["elements"].([]any)[0].(map[string]any)["text"].(string)
	// Links, not buttons, and & is escaped inside the URL.
	if !strings.Contains(ctxText, "<https://media.test/m/clip/c1.mp4?exp=1&amp;sig=abc|View clip>") ||
		!strings.Contains(ctxText, "<https://ha.test/lovelace/frigate|Dashboard>") {
		t.Errorf("context = %q", ctxText)
	}
	// When (in the viewer's timezone), how long, where, and how serious.
	for _, want := range []string{
		"<!date^" + strconv.FormatInt(reviewStart.Unix(), 10) + "^{date_short_pretty} {time}|Sep 19 21:37 UTC>",
		"42s", "Driveway", "Alert",
	} {
		if !strings.Contains(ctxText, want) {
			t.Errorf("context = %q, want it to contain %q", ctxText, want)
		}
	}

	section := blocks[1].(map[string]any)["text"].(map[string]any)
	if section["type"] != "mrkdwn" || section["text"] != "*Person detected in Driveway*\nWalking to the door." {
		t.Errorf("section = %v", section)
	}
	if body["text"] != "Front Porch: Person detected in Driveway" {
		t.Errorf("fallback text = %q, want the camera and headline for the push preview", body["text"])
	}
}

func TestSlackEscapesModelWrittenText(t *testing.T) {
	srv := newServer(t, reply{status: 200, body: `{"ok":true,"channel":"C1","ts":"1.1"}`})

	m := Message{
		Title: "A & B", Headline: "<!channel> Tom & Jerry", Detail: "<@U123> says *hi* and `rm` _now_ ~x~", Tag: "x",
		Body: "unused",
	}
	if _, err := newSlack(srv).Send(context.Background(), config.Target{Channel: "C1"}, m, ""); err != nil {
		t.Fatal(err)
	}

	body := srv.only(t).json(t)
	blocks := body["blocks"].([]any)
	section := blocks[1].(map[string]any)["text"].(map[string]any)["text"].(string)
	if want := "*&lt;!channel&gt; Tom &amp; Jerry*\n&lt;@U123&gt; says ∗hi∗ and ˋrmˋ ‗now‗ ∼x∼"; section != want {
		t.Errorf("section = %q, want mentions neutralized and formatting marks defused: %q", section, want)
	}
	if strings.Contains(body["text"].(string), "<!channel>") {
		t.Errorf("fallback text %q would still ping", body["text"])
	}
	// plain_text is shown literally, so the header must not be entity-escaped.
	if header := blocks[0].(map[string]any)["text"].(map[string]any)["text"]; header != "A & B" {
		t.Errorf("header = %q, want it unescaped", header)
	}
}

// Escaping runs after cutting to length, or an entity could be cut in half.
func TestSlackDoesNotCutAnEntityInHalf(t *testing.T) {
	srv := newServer(t, reply{status: 200, body: `{"ok":true,"channel":"C1","ts":"1.1"}`})

	m := started()
	m.Detail = strings.Repeat("&", 5000)
	if _, err := newSlack(srv).Send(context.Background(), config.Target{Channel: "C1"}, m, ""); err != nil {
		t.Fatal(err)
	}
	blocks := srv.only(t).json(t)["blocks"].([]any)
	section := blocks[1].(map[string]any)["text"].(map[string]any)["text"].(string)
	if len([]rune(section)) > 3000 {
		t.Errorf("section = %d runes, want at most Slack's 3000", len([]rune(section)))
	}
	if strings.Contains(strings.ReplaceAll(section, "&amp;", ""), "&") {
		t.Errorf("section ends in a cut entity: %q", section[len(section)-12:])
	}
}

func TestSlackCriticalAndLimits(t *testing.T) {
	srv := newServer(t, reply{status: 200, body: `{"ok":true,"channel":"C1","ts":"1.1"}`})

	m := Message{Title: strings.Repeat("t", 400), Headline: strings.Repeat("h", 5000), Detail: strings.Repeat("d", 5000), Tag: "x", Critical: true}
	if _, err := newSlack(srv).Send(context.Background(), config.Target{Channel: "C1"}, m, ""); err != nil {
		t.Fatal(err)
	}

	body := srv.only(t).json(t)
	blocks := body["blocks"].([]any)
	header := blocks[0].(map[string]any)["text"].(map[string]any)["text"].(string)
	section := blocks[1].(map[string]any)["text"].(map[string]any)["text"].(string)
	if !strings.HasPrefix(header, "🚨 ") || len([]rune(header)) > 150 {
		t.Errorf("header = %d runes %q..., want the alarm prefix and Slack's 150 limit", len([]rune(header)), header[:10])
	}
	if len([]rune(section)) > 3000 {
		t.Errorf("section = %d runes, want at most Slack's 3000", len([]rune(section)))
	}
	if len([]rune(body["text"].(string))) > 4000 {
		t.Error("fallback text is over the 4000 Slack recommends")
	}
}

// An edit keeps critical styling.
func TestSlackLayoutBySituation(t *testing.T) {
	critical := ended()
	critical.Critical = true

	tests := []struct {
		name       string
		msg        Message
		wantBlocks string
		wantHeader string
		wantCtx    []string
	}{
		{"new alert", started(), "header,section,image,context", "Front Porch", []string{"Driveway", "Alert", "|Dashboard>"}},
		{"critical alert", func() Message { m := started(); m.Critical = true; return m }(), "header,section,image,context", "🚨 Front Porch", nil},
		{"ended update", ended(), "header,section,image,context", "Front Porch", []string{"42s", "View clip"}},
		{"critical update", critical, "header,section,image,context", "🚨 Front Porch", nil},
		{"digest", digest(), "header,section,context", "Front Porch", []string{"Dashboard"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := newSlack(newServer(t)).render("C1", "", tc.msg, true)
			var types []string
			for _, b := range body.Blocks {
				types = append(types, b.Type)
			}
			if got := strings.Join(types, ","); got != tc.wantBlocks {
				t.Errorf("blocks = %s, want %s", got, tc.wantBlocks)
			}
			if body.Blocks[0].Text.Text != tc.wantHeader {
				t.Errorf("header = %q, want %q", body.Blocks[0].Text.Text, tc.wantHeader)
			}
			ctx := body.Blocks[len(body.Blocks)-1].Elements[0].Text
			for _, want := range tc.wantCtx {
				if !strings.Contains(ctx, want) {
					t.Errorf("context = %q, want %q", ctx, want)
				}
			}
			if tc.name == "digest" && strings.Contains(ctx, "<!date") {
				t.Errorf("a digest has no start time, got %q", ctx)
			}
		})
	}
}

func TestSlackEditsInPlaceUsingTheStoredChannel(t *testing.T) {
	// chat.update replies without a channel or ts of its own worth trusting.
	srv := newServer(t, reply{status: 200, body: `{"ok":true}`})

	ref, err := newSlack(srv).Send(context.Background(), config.Target{Channel: "U123"}, message, "D999:1700.0001")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ref != "D999:1700.0001" {
		t.Errorf("ref = %q, want the same message", ref)
	}

	req := srv.only(t)
	if req.path != "/chat.update" {
		t.Fatalf("path = %s, want chat.update", req.path)
	}
	body := req.json(t)
	// The DM's D... id, not the configured user id: chat.update rejects a user id.
	if body["channel"] != "D999" || body["ts"] != "1700.0001" {
		t.Errorf("channel/ts = %v / %v", body["channel"], body["ts"])
	}
	// Sent together, or the old blocks would linger.
	if body["text"] == "" || len(body["blocks"].([]any)) == 0 {
		t.Error("update must carry both text and blocks")
	}
}

func TestSlackPostsFreshWhenTheMessageWasDeleted(t *testing.T) {
	srv := newServer(t,
		reply{status: 200, body: `{"ok":false,"error":"message_not_found"}`},
		reply{status: 200, body: `{"ok":true,"channel":"C1","ts":"9.9"}`},
	)

	ref, err := newSlack(srv).Send(context.Background(), config.Target{Channel: "C1"}, message, "C1:1.1")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ref != "C1:9.9" {
		t.Errorf("ref = %q, want the fresh message", ref)
	}
	reqs := srv.all()
	if len(reqs) != 2 || reqs[0].path != "/chat.update" || reqs[1].path != "/chat.postMessage" {
		t.Errorf("requests = %+v, want update then post", reqs)
	}
}

func TestSlackOtherEditErrorsAreNotPostedAround(t *testing.T) {
	srv := newServer(t, reply{status: 200, body: `{"ok":false,"error":"cant_update_message"}`})

	_, err := newSlack(srv).Send(context.Background(), config.Target{Channel: "C1"}, message, "C1:1.1")
	if err == nil || !strings.Contains(err.Error(), "cant_update_message") {
		t.Errorf("err = %v", err)
	}
	if got := len(srv.all()); got != 1 {
		t.Errorf("requests = %d, want no fallback post for an error that isn't a missing message", got)
	}
}

// A post retries without the image if Slack can't fetch it.
func TestSlackRetriesWithoutTheImageWhenSlackCannotFetchIt(t *testing.T) {
	srv := newServer(t,
		reply{status: 200, body: `{"ok":false,"error":"invalid_blocks"}`},
		reply{status: 200, body: `{"ok":true,"channel":"C1","ts":"2.2"}`},
	)

	ref, err := newSlack(srv).Send(context.Background(), config.Target{Channel: "C1"}, message, "")
	if err != nil || ref != "C1:2.2" {
		t.Fatalf("Send = %q, %v", ref, err)
	}
	reqs := srv.all()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want the failed try and the retry", len(reqs))
	}
	if !strings.Contains(strings.Join(blockTypes(reqs[0].json(t)), ","), "image") {
		t.Error("first attempt should include the image")
	}
	if strings.Contains(strings.Join(blockTypes(reqs[1].json(t)), ","), "image") {
		t.Error("retry should drop the image block")
	}
}

// An edit must not retry without the image: that strips the one showing.
func TestSlackEditDoesNotDropTheImage(t *testing.T) {
	srv := newServer(t, reply{status: 200, body: `{"ok":false,"error":"invalid_blocks"}`})

	_, err := newSlack(srv).Send(context.Background(), config.Target{Channel: "C1"}, ended(), "C1:1.1")
	if err == nil || !strings.Contains(err.Error(), "invalid_blocks") {
		t.Errorf("err = %v, want the rejection reported", err)
	}
	if got := len(srv.all()); got != 1 {
		t.Errorf("requests = %d, want no retry that would strip the image", got)
	}
}

func TestSlackInvalidBlocksWithoutAnImageIsAnError(t *testing.T) {
	srv := newServer(t, reply{status: 200, body: `{"ok":false,"error":"invalid_blocks"}`})

	m := message
	m.Image = ""
	if _, err := newSlack(srv).Send(context.Background(), config.Target{Channel: "C1"}, m, ""); err == nil {
		t.Error("nothing to drop, so this is a real error")
	}
	if got := len(srv.all()); got != 1 {
		t.Errorf("requests = %d, want no retry loop", got)
	}
}

func TestSlackRetriesOnceWhenRateLimited(t *testing.T) {
	srv := newServer(t,
		reply{status: 429, header: map[string]string{"Retry-After": "0.01"}},
		reply{status: 200, body: `{"ok":true,"channel":"C1","ts":"3.3"}`},
	)
	if ref, err := newSlack(srv).Send(context.Background(), config.Target{Channel: "C1"}, message, ""); err != nil || ref != "C1:3.3" {
		t.Fatalf("Send = %q, %v", ref, err)
	}

	limited := newServer(t, reply{status: 429, header: map[string]string{"Retry-After": "0.01"}})
	if _, err := newSlack(limited).Send(context.Background(), config.Target{Channel: "C1"}, message, ""); err == nil {
		t.Error("a second 429 should be reported, not retried forever")
	}
	if got := len(limited.all()); got != 2 {
		t.Errorf("requests = %d, want exactly one retry", got)
	}

	tooLong := newServer(t, reply{status: 429, header: map[string]string{"Retry-After": "120"}})
	if _, err := newSlack(tooLong).Send(context.Background(), config.Target{Channel: "C1"}, message, ""); err == nil {
		t.Error("a long Retry-After should fail fast rather than hold the alert")
	}
	if got := len(tooLong.all()); got != 1 {
		t.Errorf("requests = %d, want no retry when the wait is too long", got)
	}
}

func TestSlackMalformedRef(t *testing.T) {
	srv := newServer(t)
	if _, err := newSlack(srv).Send(context.Background(), config.Target{Channel: "C1"}, message, "garbage"); err == nil {
		t.Error("a malformed ref should be an error, not a silent new post")
	}
}

func TestSlackVerify(t *testing.T) {
	ok := newServer(t, reply{status: 200, body: `{"ok":true}`})
	if err := newSlack(ok).Verify(context.Background(), nil); err != nil {
		t.Errorf("Verify = %v", err)
	}
	bad := newServer(t, reply{status: 200, body: `{"ok":false,"error":"invalid_auth"}`})
	if err := newSlack(bad).Verify(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("Verify = %v, want invalid_auth", err)
	}
}

// ---- Discord ----

func discordTarget(srv *server, query string) config.Target {
	return config.Target{Type: config.TargetDiscord, WebhookURL: srv.URL + "/api/webhooks/1234/secrettoken" + query}
}

func TestDiscordPostsAnEmbedAndReturnsTheMessageID(t *testing.T) {
	srv := newServer(t, reply{status: 200, body: `{"id":"555"}`})

	m := ended()
	m.Update = false
	m.Critical = true
	ref, err := NewDiscord().Send(context.Background(), discordTarget(srv, ""), m, "")
	if err != nil || ref != "555" {
		t.Fatalf("Send = %q, %v", ref, err)
	}

	req := srv.only(t)
	if req.method != "POST" || req.path != "/api/webhooks/1234/secrettoken" || req.query != "wait=true" {
		t.Errorf("request = %s %s?%s, want a POST with wait=true so the id comes back", req.method, req.path, req.query)
	}
	// An empty (not null) parse list is what disables every mention.
	if !strings.Contains(string(req.body), `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("body = %s, want allowed_mentions.parse to be an empty list", req.body)
	}

	body := req.json(t)
	// The content is the push preview, so it says what happened.
	if body["content"] != "**🚨 Front Porch** · Person detected in Driveway" || body["username"] != "Frigate" {
		t.Errorf("content/username = %q / %q", body["content"], body["username"])
	}
	if _, ok := body["flags"]; ok {
		t.Error("an alert must notify: no suppress flag")
	}

	embed := body["embeds"].([]any)[0].(map[string]any)
	if embed["color"] != float64(discordColorCritical) || embed["timestamp"] != "2026-09-19T21:37:00Z" {
		t.Errorf("color/timestamp = %v / %v", embed["color"], embed["timestamp"])
	}
	if embed["image"].(map[string]any)["url"] != m.Image {
		t.Errorf("image = %v", embed["image"])
	}
	if got := embed["footer"].(map[string]any)["text"]; got != "Driveway · Alert · 42s" {
		t.Errorf("footer = %q, want where, how serious and how long", got)
	}
	want := "Walking to the door.\n\n[▶ View clip](https://media.test/m/clip/c1.mp4?exp=1&sig=abc) · [Dashboard](https://ha.test/lovelace/frigate)"
	if embed["description"] != want {
		t.Errorf("description = %q, want %q", embed["description"], want)
	}
	if _, ok := embed["title"]; ok {
		t.Error("the headline is in the content; a title would repeat it")
	}
}

func TestDiscordColorFollowsSeverityAndThreat(t *testing.T) {
	d := NewDiscord()
	for name, tc := range map[string]struct {
		mutate func(*Message)
		want   int
	}{
		"alert":      {func(m *Message) {}, discordColorAlert},
		"detection":  {func(m *Message) { m.Severity = "detection" }, discordColorDetection},
		"suspicious": {func(m *Message) { m.Threat = 1 }, discordColorSuspect},
		"threat":     {func(m *Message) { m.Threat = 2 }, discordColorCritical},
		"critical":   {func(m *Message) { m.Critical = true }, discordColorCritical},
	} {
		m := started()
		tc.mutate(&m)
		if got := d.render(m).Embeds[0].Color; got != tc.want {
			t.Errorf("%s: color = %#x, want %#x", name, got, tc.want)
		}
	}
}

// A running count is not worth a buzz, and it has no picture or start time.
func TestDiscordDigestIsSilent(t *testing.T) {
	p := NewDiscord().render(digest())
	if p.Flags != discordSuppressNotifications {
		t.Errorf("flags = %d, want notifications suppressed", p.Flags)
	}
	if p.Content != "**Front Porch** · 3 more events since 21:37" {
		t.Errorf("content = %q", p.Content)
	}
	e := p.Embeds[0]
	if e.Image != nil || e.Timestamp != "" || e.Footer != nil {
		t.Errorf("embed = %+v, want none of a picture, time or footer", e)
	}
}

// An embed with nothing in it is a 400. Sent as [] so an edit still clears one.
func TestDiscordOmitsAnEmptyEmbed(t *testing.T) {
	m := digest()
	m.ClickURL = ""
	body, err := json.Marshal(NewDiscord().render(m))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"embeds":[]`) {
		t.Errorf("body = %s, want an empty embeds list", body)
	}
}

func TestDurationNeedsBothEnds(t *testing.T) {
	m := ended()
	m.Start = time.Time{}
	if got := duration(m); got != "" {
		t.Errorf("duration = %q with no start, want none", got)
	}
	if ctx := slackContext(m); strings.Contains(ctx, "h ") || strings.Contains(ctx, "s ") {
		t.Errorf("context = %q, want no duration", ctx)
	}
	m = ended()
	m.End = time.Time{}
	if got := duration(m); got != "" {
		t.Errorf("duration = %q while ongoing, want none", got)
	}
}

func TestDiscordStaysWithinTheLimits(t *testing.T) {
	m := started()
	m.Headline = strings.Repeat("h", 5000)
	m.Detail = strings.Repeat("d", 9000)
	m.Zones = []string{strings.Repeat("z", 5000)}
	p := NewDiscord().render(m)

	e := p.Embeds[0]
	if n := len([]rune(p.Content)); n > 2000 {
		t.Errorf("content = %d runes, want at most 2000", n)
	}
	total := len([]rune(e.Description)) + len([]rune(e.Footer.Text))
	if total > 6000 {
		t.Errorf("embed text = %d runes, want at most Discord's 6000", total)
	}
}

func TestDiscordEditReplacesTheMessageAndKeepsTheThread(t *testing.T) {
	srv := newServer(t, reply{status: 200, body: `{"id":"555"}`})

	ref, err := NewDiscord().Send(context.Background(), discordTarget(srv, "?thread_id=77"), message, "555")
	if err != nil || ref != "555" {
		t.Fatalf("Send = %q, %v", ref, err)
	}

	req := srv.only(t)
	if req.method != "PATCH" || req.path != "/api/webhooks/1234/secrettoken/messages/555" {
		t.Errorf("request = %s %s", req.method, req.path)
	}
	if req.query != "thread_id=77" {
		t.Errorf("query = %q, want thread_id kept and no wait", req.query)
	}
}

func TestDiscordPostsFreshWhenTheMessageWasDeleted(t *testing.T) {
	srv := newServer(t,
		reply{status: 404, body: `{"code":10008}`},
		reply{status: 200, body: `{"id":"777"}`},
	)

	ref, err := NewDiscord().Send(context.Background(), discordTarget(srv, ""), message, "555")
	if err != nil || ref != "777" {
		t.Fatalf("Send = %q, %v", ref, err)
	}
	reqs := srv.all()
	if len(reqs) != 2 || reqs[0].method != "PATCH" || reqs[1].method != "POST" {
		t.Errorf("requests = %+v, want PATCH then POST", reqs)
	}
}

func TestDiscordEscapesMarkdownFromModelText(t *testing.T) {
	srv := newServer(t, reply{status: 200, body: `{"id":"1"}`})

	m := started()
	m.Headline = "**bold** @everyone"
	m.Detail = "**bold** [phish](http://evil.test) @everyone `x` <@123> <t:1:R>\n# heading\n- item\n-# small"
	if _, err := NewDiscord().Send(context.Background(), discordTarget(srv, ""), m, ""); err != nil {
		t.Fatal(err)
	}

	body := srv.only(t).json(t)
	desc := body["embeds"].([]any)[0].(map[string]any)["description"].(string)
	for _, bad := range []string{"[phish](", "**bold**", "<@123>", "<t:1:R>", "\n# ", "\n- ", "\n-# "} {
		if strings.Contains(desc, bad) {
			t.Errorf("description = %q, want %q neutralized", desc, bad)
		}
	}
	if strings.Contains(body["content"].(string), "**bold**") {
		t.Errorf("content = %q, want model text escaped", body["content"])
	}
}

func TestDiscordDropsTheImageOnEditWhenThereIsNone(t *testing.T) {
	srv := newServer(t, reply{status: 200, body: `{}`})

	m := message
	m.Image = ""
	if _, err := NewDiscord().Send(context.Background(), discordTarget(srv, ""), m, "555"); err != nil {
		t.Fatal(err)
	}
	// An edit replaces the embeds, so an absent image really is removed.
	embed := srv.only(t).json(t)["embeds"].([]any)[0].(map[string]any)
	if _, ok := embed["image"]; ok {
		t.Error("image should be omitted from the edit")
	}
}

func TestDiscordRetriesOnceWhenRateLimited(t *testing.T) {
	srv := newServer(t,
		reply{status: 429, header: map[string]string{"Retry-After": "0.01"}},
		reply{status: 200, body: `{"id":"9"}`},
	)
	if ref, err := NewDiscord().Send(context.Background(), discordTarget(srv, ""), message, ""); err != nil || ref != "9" {
		t.Fatalf("Send = %q, %v", ref, err)
	}
}

// The webhook URL is the credential: keep it out of errors.
func TestDiscordErrorsNeverContainTheWebhookToken(t *testing.T) {
	down := newServer(t)
	target := discordTarget(down, "")
	down.Close() // a transport error: net/http would embed the full URL

	_, err := NewDiscord().Send(context.Background(), target, message, "")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "secrettoken") {
		t.Errorf("error leaks the webhook token: %v", err)
	}

	rejected := newServer(t, reply{status: 400, body: `{"message":"Invalid Form Body"}`})
	_, err = NewDiscord().Send(context.Background(), discordTarget(rejected, ""), message, "")
	if err == nil || strings.Contains(err.Error(), "secrettoken") || !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v, want the status without the token", err)
	}
}

func TestDiscordVerifyNamesBadWebhooksWithoutTheirTokens(t *testing.T) {
	good := newServer(t, reply{status: 200, body: `{}`})
	gone := newServer(t, reply{status: 404})

	err := NewDiscord().Verify(context.Background(), []config.Target{discordTarget(good, ""), discordTarget(gone, "")})
	if err == nil {
		t.Fatal("a deleted webhook should fail Verify")
	}
	if strings.Contains(err.Error(), "secrettoken") || !strings.Contains(err.Error(), "discord:1234") {
		t.Errorf("err = %v, want the webhook id and not the token", err)
	}
	if err := NewDiscord().Verify(context.Background(), []config.Target{discordTarget(good, "")}); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// ---- registry ----

func TestNewRegistersOnlyConfiguredBackends(t *testing.T) {
	bare := New(&config.Config{}, &fakeHassClient{})
	if _, ok := bare[config.TargetSlack]; ok {
		t.Error("slack registered without a bot token")
	}
	if _, ok := bare[config.TargetNtfy]; ok {
		t.Error("ntfy registered without a server")
	}
	if _, ok := bare[config.TargetHass]; !ok {
		t.Error("hass missing")
	}
	if _, ok := bare[config.TargetDiscord]; !ok {
		t.Error("discord missing: a webhook carries its own credential")
	}

	full := New(&config.Config{
		Slack: config.SlackConfig{BotToken: "x"},
		Ntfy:  config.NtfyConfig{URL: "https://ntfy.test"},
	}, &fakeHassClient{})
	if len(full) != 4 {
		t.Errorf("registry has %d senders, want 4", len(full))
	}

	if _, err := bare.For(config.Target{Type: config.TargetSlack}); err == nil {
		t.Error("For should report a target type with no sender")
	}
}

func TestRegistryVerifyCollectsEachBackendsFindings(t *testing.T) {
	ntfyDown := newServer(t, reply{status: 500})
	reg := Registry{
		config.TargetHass: NewHass(&fakeHassClient{}, ""), // a client that can't list services: skipped
		config.TargetNtfy: NewNtfy(config.NtfyConfig{URL: ntfyDown.URL}),
	}
	errs := reg.Verify(context.Background(), map[string]config.Recipient{
		"alice": {Targets: []config.Target{{Type: config.TargetHass, Service: "x"}, {Type: config.TargetNtfy, Topic: "t"}}},
	})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "ntfy") {
		t.Errorf("errs = %v, want just the ntfy failure", errs)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate = %q", got)
	}
	got := truncate(strings.Repeat("é", 20), 5)
	if len([]rune(got)) != 5 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate = %q, want 5 runes ending in an ellipsis", got)
	}
}
