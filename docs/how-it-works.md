# How it works

Frigate Notifications replaces the `SgtBatten/HA_blueprints` Frigate
blueprint and the four UI automations it drove, and delivers through Home
Assistant, Slack, ntfy, and Discord.

It subscribes to Frigate's MQTT review stream, decides what to send with an
ordered rule list, and fans each notification out to named recipients, each
reachable on one or more backends. Media (snapshots, clips, previews) is served
through a signed proxy in this service, because phones off the home network
can't authenticate to Frigate directly.

The blueprint needed one automation per person, so the day/night rules were
duplicated four ways and drifted apart. Here a rule names recipients and each
recipient carries their own policy (active hours, critical opt-in, rate cap), so
there is one place to change a rule.

## Media proxy

Frigate is split-horizon: its in-cluster listener has no auth, its public one
needs a session a push notification can't carry. Notifications therefore link
to `frigate-notifications.example.com`, served by this service on **:8081**, which
fetches from Frigate in-cluster on the phone's behalf.

Links look like `/m/<kind>/<id>.<ext>?exp=…&sig=…`: a snapshot (`jpg`), preview
(`gif`), clip (`mp4`) or clip player page (`html`). The extension is unsigned
and only there because phones infer a file's type from it (the Android app
animates a GIF only when the path ends in `gif`). Links are
`HMAC-SHA256(kind/id/exp)`, verified in constant time, and expire after
`media.linkTTL`. They're stateless on purpose: a link that needed a
datastore lookup would break every outstanding notification's image during a
Valkey outage. Rotating `media.signingKey` invalidates all outstanding links,
which is the only revocation with a realistic trigger.

"View Clip" opens `/m/play/<clip>.html`, a page that plays Frigate's HLS
(`/vod`) where the browser can, as iOS and Android do, and the mp4 elsewhere:
Safari can't reliably play Frigate's mp4, which is streamed with no length or
byte ranges. The HLS files are served from `/m/vod/<clip>/<exp>/<sig>/<file>`,
signed in the path because the playlists link their files relatively, which
drops a query string.

Health checks live on **:8080** and are never routed publicly — whatever port
is public exposes every handler bound to it. Metrics are pushed to OTel, not
served.

On Kubernetes the chart can route the media port for you; see
[Exposing media](kubernetes.md#exposing-media).

## State

Cooldowns, per-review notification state, rate caps, digests and the MQTT
redelivery guard are persisted, because CI restarts this deployment on every
image push and in-process state would drop every live cooldown (a notification
storm on the next motion) and every pending end-of-review update. Where they
live, in order of preference:

1. **Valkey**, if `redis.url` is set.
2. **The database**, if `db.url` is set and `redis.url` isn't (a `kv` table /
   collection with per-entry expiry).
3. **Process memory**, if neither is set — correct, but forgotten on restart.

`db.url` is a MongoDB (`mongodb://`, `mongodb+srv://`) or PostgreSQL
(`postgres://`, `postgresql://`) URL; the scheme picks the backend. Either way
it also holds the write-only audit log (`internal/eventstore`): every review
and GenAI description event plus every notification delivery attempt, kept
for a year. Postgres stores these as `mqtt_events` / `notifications` tables
with indexed columns for the fields queried and the whole record as JSONB;
tables are created on startup and old rows are swept hourly. Mongo uses TTL
indexes instead. The audit log is history only — never a decision input.

**Every store operation fails open.** If the primary store is unreachable the
service falls back to an in-process map, logs, and increments
`frigate_notify_store_fallback_total`. A missing cooldown costs an extra
notification; the alternative — a security system muting itself when its cache
dies — is never acceptable. Audit writes are detached and best-effort: a
database outage never blocks or fails a notification.
