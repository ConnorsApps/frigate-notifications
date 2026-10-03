# How it works

Frigate Notifications subscribes to Frigate's MQTT review stream, decides what
to send with an ordered rule list, and fans each notification out to named
recipients, each reachable on one or more backends. Rules name recipients and
each recipient carries their own policy (active hours, critical opt-in, rate
cap), so there's one place to change a rule. Media is served through a signed
proxy in this service, because phones off the home network can't authenticate
to Frigate.

## Media proxy

Frigate is split-horizon: its in-cluster listener has no auth, and its public
one needs a session a push notification can't carry. So notifications link to
this service's media port, **:8081** (at `media.publicBaseURL`), which fetches
from Frigate on the phone's behalf.

Links look like `/m/<kind>/<id>.<ext>?exp=…&sig=…`: a snapshot (`jpg`), preview
(`gif`), clip (`mp4`) or clip player page (`html`). The extension is unsigned;
phones infer a file's type from it (Android animates a GIF only when the path
ends in `gif`). Links are `HMAC-SHA256(kind/id/exp)`, verified in constant time,
and expire after `media.linkTTL`. They're stateless, so a Valkey outage can't
break images; rotating `media.signingKey` invalidates every outstanding link.

"View Clip" opens `/m/play/<clip>.html`, which plays Frigate's HLS (`/vod`)
where the browser can, as iOS and Android do, and the mp4 elsewhere: Safari
can't reliably play Frigate's mp4, which is streamed without length or byte
ranges. HLS files are served from `/m/vod/<clip>/<exp>/<sig>/<file>`, signed in
the path because the playlists link their files relatively.

Health checks are on **:8080**, which is never routed publicly. Metrics are
pushed to OTel, not served. On Kubernetes the chart can route the media port;
see [Exposing media](kubernetes.md#exposing-media).

## State

Cooldowns, per-review state, rate caps, digests and the MQTT redelivery guard
are persisted, so a restart doesn't cause a notification storm or lose pending
end-of-review updates. In order of preference, they live in:

1. **Valkey**, if `redis.url` is set.
2. **The database**, if `db.url` is set and `redis.url` isn't (a `kv` table /
   collection with per-entry expiry).
3. **Process memory**, if neither is set: correct, but forgotten on restart.

`db.url` is a MongoDB (`mongodb://`, `mongodb+srv://`) or PostgreSQL
(`postgres://`, `postgresql://`) URL; the scheme picks the backend. It also
holds the write-only audit log: every review and GenAI event plus every
delivery attempt, kept for a year. Postgres keeps these in `mqtt_events` /
`notifications` tables (indexed columns plus the whole record as JSONB),
created on startup and swept hourly; Mongo uses TTL indexes. The audit log is
history only, never a decision input.

**Every store operation fails open.** If the store is unreachable, the service
falls back to memory, logs, and increments `frigate_notify_store_fallback_total`:
an extra notification beats a security system that mutes itself. Audit writes
are best-effort and never block a notification.
