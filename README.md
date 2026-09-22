# frigate-notify

Frigate → notification bridge, replacing the `SgtBatten/HA_blueprints` Frigate
blueprint and the four UI automations it drove. Delivers through Home
Assistant, Slack, ntfy, and Discord.

Subscribes to Frigate's MQTT review stream, decides what to send with an
ordered rule list, and fans each notification out to named recipients, each
reachable on one or more backends. Media (snapshots, clips, previews) is served
through a signed proxy in this service, because phones off the home network
can't authenticate to Frigate directly.

The blueprint needed one automation per person, so the day/night rules were
duplicated four ways and drifted apart. Here a rule names recipients and each
recipient carries their own policy (active hours, critical opt-in, rate cap), so
there is one place to change a rule.

## Rules

Rules are ordered and the first match wins, so **position is priority**. Each
rule ANDs its `when` conditions; each list within one is an OR; anything
omitted matches everything.

```yaml
rules:
  - name: night-person
    when:
      hours: { from: dusk+30m, to: dawn-30m }
      labels: [person]
      severity: [alert]
      excludeSubLabels: [alice, bob]   # don't alert when it's only household faces
    unless:
      - entityState: { alarm_control_panel.home: disarmed }
      - entityState: { input_boolean.guest_mode: 'on' }
    holdoff: 5s
    cooldown: 5m
    cooldownScope: rule
    to: [alice, bob]
    preset: liveview
    critical: true
```

See `config-example.yaml` for the full shape and `config.schema.json` for
editor validation.

### Time windows

`hours` and `activeHours` take `from` and `to`, each independently a quoted
wall-clock `"HH:MM"`, a 12-hour time (`9am`, `11:30pm`), `noon`/`midnight`, or
a solar event — `sunrise`, `sunset`, `dawn`, `dusk` — with an optional offset
(`dusk+30m`, `dawn-30m`). A `to` earlier than `from` wraps midnight.

Solar endpoints resolve from Home Assistant's `sun.sun`. A fixed 23:00–06:00
night window is wrong for half the year: in December it is fully dark by 17:00,
and a person-only critical rule wouldn't apply for six hours of darkness.

Quote 24-hour wall-clock times: bare `06:00` is a YAML sexagesimal integer.
`9am`, `noon`, and the solar forms don't need quoting.

### Rule timezone

Every rule evaluates `hours` in the config's top-level `timezone` by default.
Set `timezone` on a rule to override that for just its `when`/`unless` hours
windows, e.g. a rule that should key off a camera or person in a different
timezone:

```yaml
rules:
  - name: remote-camera-daytime
    timezone: America/Los_Angeles
    when:
      hours: { from: 9am, to: 6pm }
      cameras: [warehouse_west]
    to: [alice]
```

### `unless`

`unless` is a list of the same condition blocks as `when`. **Any** entry
matching suppresses the rule; conditions *within* one entry are ANDed. It is
the place for "don't tell me when we're home and awake", which sub-label
exclusion can't express.

### Lifecycle

Frigate reports a review three times, and all three matter, then once more if
GenAI review summaries are on:

| phase | what happens |
|---|---|
| `new` | wait out any `holdoff`, match, check the cooldown, send |
| `update` | re-match. Frigate escalates a review from `detection` to `alert` mid-life, so a `severity: [alert]` rule must still be able to fire here. Frigate publishes an update on every change, so the MQTT redelivery guard keys on the payload's content: keying on the phase would drop every update after the first, escalation included |
| `end` | re-match. Same rule, or a higher-priority rule that isn't more critical → refresh the notification in place (same tag) with the clip and any GenAI description. A higher-priority rule that raises the alert to **critical** → a fresh notification under a new tag (`-esc`), because Android's `alert_once` and ntfy's sequence replace are both silent and reusing the tag would make the one message meant to wake someone the one that doesn't. The earlier notification stays |
| `genai` | Published some time after `end`, once Frigate's GenAI summary (`data.metadata`: `title`, `shortSummary`, `potential_threat_level`) is ready (Frigate ≥ 0.17, `review.genai` on). Not a rule phase: it quietly edits the notification already delivered, with the title as headline and the summary as detail. Threat level 1 prefixes "Needs review:", 2 "Security concern:", as Frigate does; it is shown, never acted on. With no earlier notification it does nothing |

A re-match on a later phase re-pushes **only to raise criticality**. A
higher-priority rule that merely adds detail (a zone, a sub-label) refreshes the
notification in place instead of buzzing twice, and a lower-priority match never
downgrades a critical alert.

`holdoff` exists because face recognition is not instant: `sub_labels` is
usually empty on the `new` payload and fills in moments later, so
`excludeSubLabels` evaluated immediately would let the rule fire anyway. A
push can't be recalled, so the decision waits. Rules using sub-labels default
to 5s. The decision is made on the newest payload seen during the wait, not the
one that started it, and a review that ends during the wait is decided at once
on its end payload.

**Updates are quiet.** The clip becoming ready or a GenAI summary arriving edits
a notification that already alerted, so it doesn't alert again (mechanism per
backend in the [table below](#backends)). A critical review's update drops the
critical push, because iOS can't replace a critical notification and would ring
twice.

### Media and presets

Media is picked automatically. Each candidate is probed against Frigate
in-cluster, and the first one that exists and fits wins; if none does, the push
goes out as text. The clip and the best still (preview gif, else snapshot) are
resolved independently, and each backend shows what it can: the Home Assistant
app plays the clip on iOS and shows the still on Android (which shows only a few
frames of a video), while Slack, Discord, and ntfy can't play a clip inline, so
they get the still and the clip as a link; see [Backends](#backends).

| phase | order tried |
|---|---|
| `new` | snapshot → text |
| `end` | clip (≤25 MB), then the first still that fits: review preview gif (≤10 MB) → snapshot; text if neither |

The clip spans the whole review (`/api/<camera>/start/<s>/end/<e>/clip.mp4`, 2s
before and 3s after), not just its first detection. The clip cap is below iOS's
50 MB hard limit because the notification extension has ~30 s to download it on
whatever connection the phone has. A clip too big to attach is still linked from
the "View Clip" action.

`preset` is optional and only overrides that:

| preset | effect |
|---|---|
| `auto` (default) | the selection above |
| `text` | no attachment (the "View Clip" action is still offered) |
| `liveview` | `auto`, plus the camera's `liveViewEntity` for an iOS live stream on expand |

`frigate_notify_media_selected_total{phase,kind}` counts what each notification
carried; `kind="none"` means every candidate failed and it fell back to text
(`off` is a rule using `preset: text`). Check it first when someone
says the images stopped showing up.

`critical: true` emits both iOS (`push.interruption-level`) and Android
(`alarm_stream` channel, `ttl`/`priority`) keys in one payload; each platform
ignores the other's.

### Recipient policy

Applied after a rule matches, so a rule never needs a per-person variant. It is
applied once per recipient, not once per target: a recipient with a phone push
and a Slack DM is rate-capped as one person, and the digest goes to both:

- `allowCritical: false` downgrades a critical rule to a normal push
- `activeHours` is the awake window: outside it, non-critical pushes are
  dropped. Named for what it permits, because "quiet hours" inverts the
  meaning of every value in it
- `maxPerHour` collapses the overflow into a self-replacing digest

Critical notifications bypass both active hours and the rate cap — that is what
a recipient opted into by setting `allowCritical`.

## Backends

A recipient is a person with a list of `targets`; every target of a recipient
gets each notification. Targets are delivered concurrently, each with its own
15s timeout, so a hung Slack call never delays a Home Assistant push — or
anyone else's.

```yaml
recipients:
  alice:
    targets:
      - { type: hass, service: mobile_app_alice_phone }
      - { type: slack, channel: U0123ABCD }   # a channel id, or a user id to DM
      - { type: ntfy, topic: alice-frigate }
      - { type: discord, webhookURL: https://discord.com/api/webhooks/... }
```

| | picture | clip | critical | update (clip ready, GenAI text) |
|---|---|---|---|---|
| `hass` | still as `image` (Android) and the clip as an `attachment` (iOS) | "View Clip" action | iOS critical alert, Android `alarm_stream` channel | replaces by `tag`; Android `alert_once`, iOS passive, no sound |
| `ntfy` | still as `attach` | "View Clip" action | priority 5, 🚨 tag | replaces by `sequence_id`, priority 2 |
| `slack` | image block | link in the context line | 🚨 in the header | `chat.update` edits the message |
| `discord` | embed image | link in the embed | red embed, 🚨 in the headline | edits the message |

The layout follows the event. Every alert carries the camera, what was detected,
where (zones), when, and severity; an ended review adds how long it lasted and
the clip; a GenAI summary replaces the headline and adds its sentence. Each
backend lays that out for itself:

- **`hass`**: grouped per camera, "Alert · Zone" as the iOS subtitle, the review
  start as the Android timestamp. Alerts are time-sensitive on iOS so they get
  through a Focus mode; detections are not. A digest uses its own low-importance
  channel.
- **`ntfy`**: an emoji per object (`walking`, `dog`, `cat`, `car`, `package`,
  else `eyes`; plus 🚨 for critical), priority 4 for an alert, 3 for a detection, 5 for critical, 2
  for anything quiet. Message text is plain: markdown is not rendered on iOS.
- **`slack`**: camera as the header, the headline in bold, then the picture and
  a context line with the time (shown in the viewer's timezone), duration,
  zones and links. The push preview is "Camera: headline".
- **`discord`**: the headline is the message text, because a phone's push preview
  shows the text and not the embed; the embed carries the detail, the picture,
  a footer (zones, severity, duration) and the start time. A digest is posted
  without a notification.

"Critical" only bypasses Do Not Disturb on `hass`. Elsewhere it is styling.
`allowCritical` still decides whether a recipient gets the critical form.

`hass:` stays required even for chat-only setups, because rule conditions
(`entityState`, solar hours) read Home Assistant state.

**Slack.** Create an app with a bot token (`chat:write`) and set
`slack.botToken`. Invite the bot to each channel, or grant `chat:write.public`,
or posts fail with `not_in_channel`. A user id opens a DM. It has to be a bot
rather than an incoming webhook, because only `chat.update` can edit a message.
Slack downloads image URLs itself when the message is posted; if it can't, the
message is re-posted without the image rather than lost.

**ntfy.** Set `ntfy.url` (and `ntfy.token` if the server needs one). Updating a
notification in place needs **ntfy server ≥ 2.16** and Android app ≥ 1.22.2;
on an older server each update arrives as a second notification. The iOS app
does not replace: an update arrives as a second, passive notification. A
self-hosted server needs `upstream-base-url: https://ntfy.sh` for instant
delivery to iOS. Authenticate with an access token (`tk_...`, sent as a Bearer
token) for a write-only user on the topic, with `auth-default-access: deny-all`
on the server. The sequence id is derived from the review id with anything
outside `A-Z a-z 0-9 - _` replaced, because ntfy rejects the "." in Frigate's
ids.

**Discord.** The webhook URL is the whole credential: treat it like a token. It
is never logged, traced, or written to the audit log at runtime (only its id is).

**Reachability and privacy.** Slack and Discord fetch snapshots from the
[media proxy](#media-proxy) themselves, so `media.publicBaseURL` has to be
reachable from their servers, not just from phones. Both services also cache
what they fetch, so a snapshot posted to a channel outlives its signed link and
is visible to everyone in that channel. Point rules at shared channels
accordingly.

Text written by the model (GenAI titles, summaries, descriptions) is escaped for
Slack and Discord markup, formatting marks are defused, and Discord mentions are
disabled, so a description can't ping a channel or restyle the message.

## Media proxy

Frigate is split-horizon: its in-cluster listener has no auth, its public one
needs a session a push notification can't carry. Notifications therefore link
to `frigate-notifications.example.com`, served by this service on **:8081**, which
fetches from Frigate in-cluster on the phone's behalf.

Links look like `/m/<kind>/<id>.<ext>?exp=…&sig=…`. The extension (`jpg`, `gif`,
`mp4`) is unsigned and only there because iOS infers an attachment's type from
it. Links are `HMAC-SHA256(kind/id/exp)`, verified in constant time, and expire
after `media.linkTTL`. They're stateless on purpose: a link that needed a
datastore lookup would break every outstanding notification's image during a
Valkey outage. Rotating `media.signingKey` invalidates all outstanding links,
which is the only revocation with a realistic trigger.

Health checks live on **:8080** and are never routed publicly — the networking
chart has no path matching, so whatever port is public exposes every handler
bound to it. Metrics are pushed to OTel, not served.

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

## Config

YAML at `CONFIG_PATH` (default `config.yaml`). Parsing is strict: an unknown
key is an error, because a typo'd condition (`sevrity:`) would otherwise
silently widen a rule to match everything.

```sh
cd cmd/schema-gen && go run .   # regenerates both schemas below
```

| Schema | For |
|--------|-----|
| `config.schema.json` | Editor validation of `config.yaml` (`# yaml-language-server: $schema=...`) |
| `chart/values.schema.json` | The chart's values, including the whole config under `config`. Helm enforces it on `install`, `upgrade`, `lint` and `template`, so a typo'd key or a bad config fails before anything deploys. |

`cmd/schema-gen` is its own Go module, so the Kubernetes types it reflects
(`k8s.io/api`) stay out of the daemon's `go.mod`. It mirrors `internal/config`
and `chart/values.yaml` by hand: edit `cmd/schema-gen/config.go` or `values.go`
alongside them. The schemas reject unknown keys, so a values key missing from
the generator fails `helm lint`, and CI fails when the checked-in schemas are
stale. Rules spanning several values (Postgres vs Mongo, bundled stores vs
`config.redis.url` / `config.db.url`) live in the chart's `validate` template
for a clearer error.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CONFIG_PATH` | `config.yaml` | Path to the YAML config file |
| `VERSION` | — | Build version, reported to OTel |
| `HASS_*`, `MQTT_*`, `MEDIA_*`, `SLACK_BOT_TOKEN`, `NTFY_*`, `REDIS_URL`, `DB_URL`, `TIMEZONE` | — | Override the matching config key (`HASS_URL` → `hass.url`, `MEDIA_SIGNING_KEY` → `media.signingKey`, …); see the `env` tags in `internal/config/config.go` |

A non-empty variable beats the file and an empty one is ignored, so the chart and
the Home Assistant app can inject secrets. A required value must come from one or
the other. Settings without an `env` tag come only from the file.

## Deploying

CI publishes both artifacts to GHCR on every push to `main`:

| Artifact | Reference |
|----------|-----------|
| Image | `ghcr.io/connorsapps/frigate-notifications` (`latest`, `<version>` from `v*` tags, `sha-<short>`) |
| Helm chart | `oci://ghcr.io/connorsapps/charts/frigate-notifications` |

```sh
helm upgrade --install frigate-notifications oci://ghcr.io/connorsapps/charts/frigate-notifications \
  --version <chart version> --set image.tag=<version> \
  --values values.yaml   # config: <the full config.yaml under the `config` key>
```

The chart renders `config` into a Secret mounted at
`/etc/frigate-notify/config.yaml`. Keep the real config (tokens, signing key,
URLs) out of git. Chart changes need a `version` bump in `chart/Chart.yaml`;
the publish job skips versions that already exist.

`scripts/` holds Kubernetes ops helpers (`kubectl`/`mongosh` against a running
deployment). They default to namespace `frigate`; override with `NS`,
`MONGO_NS`, and `MONGO_POD` (the last two only apply to `mongosh.sh`, which
needs a MongoDB `db.url`).

### Bundled backing services

The chart can deploy the state and audit stores for you. All are off by
default; when enabled the chart sets `REDIS_URL` / `DB_URL`, so leave
`config.redis.url` / `config.db.url` unset (the render fails if both are
given).

| Values | Deploys | Prerequisite |
|--------|---------|--------------|
| `valkey.enabled` | Valkey `StatefulSet` (no auth, cluster-internal) | none |
| `postgres.enabled` | CloudNativePG `Cluster` | [CloudNativePG](https://cloudnative-pg.io) operator |
| `mongo.enabled` | Official `mongo` image `StatefulSet` | none |
| `mongo.enabled` + `mongo.mode: operator` | `MongoDBCommunity` resource | MongoDB Controllers for Kubernetes operator (`mongodb/mongodb-kubernetes`) |

Each block also takes `extraSpec`, deep-merged over the rendered resource's
spec (maps merge, lists replace), plus `extraEnv` (and `valkey.extraArgs`), so
any operator or StatefulSet field can be set from values without forking, e.g.
`postgres.extraSpec.postgresql.parameters.max_connections`.

Only one of `postgres` / `mongo` (the app has a single `db.url`). State follows
the usual precedence: Valkey, else the database, else memory. The chart does
not install operators. `scripts/db-uri.sh` reads `db.url` from the config
Secret, so it has nothing to read when the URL comes from a bundled service.

### Testing the chart

Chart tests live in `chart/tests` and run with
[helm-unittest](https://github.com/helm-unittest/helm-unittest):

```sh
helm plugin install https://github.com/helm-unittest/helm-unittest --verify=false
helm unittest chart
```

### Telemetry

Traces and metrics are exported over OTLP only when an endpoint is configured;
otherwise the app exports nothing. The chart's `otel` values render the
standard `OTEL_*` environment variables (mapping in `chart/values.yaml`):

```yaml
# chart values
otel:
  endpoint: http://collector.example.svc:4318   # base URL for both signals
```

Over HTTP the SDK appends `/v1/traces` and `/v1/metrics` to the base endpoint.
For a backend that serves other paths, or to split the signals, use the
per-signal endpoints, which are used verbatim:

```yaml
otel:
  metrics:
    endpoint: http://collector.example.svc:8480/insert/0/opentelemetry/v1/metrics
  traces:
    endpoint: http://collector.example.svc:4317
    protocol: grpc
    sampler: parentbased_traceidratio
    samplerArg: "0.1"
```

Metrics are OTLP/HTTP only; traces may use `grpc`. Put secret header values in
`extraEnv` with `valueFrom` rather than `otel.headers`. Anything in `extraEnv`
replaces the generated variable of the same name, and any other `OTEL_*`
variable the SDK understands can be set there too.

## Running locally

```sh
cp config-example.yaml config.yaml
# fill in hass.token, mqtt.password, and media.signingKey (hex)
go run ./cmd/server
```

Set `dryRun: true` to build and log each backend's full payload without
calling any of them.

`cmd/notify-test` sends one real notification to a recipient's targets, or to one
backend with `--target slack|ntfy|discord|hass`. It answers what replay can't:
whether the message actually shows up, image and all. `--update-after 10s` then
sends the end-of-review update to the same message, which is how to check on
your own devices that each backend edits in place and stays quiet.

## Replaying a decision

`cmd/replay` answers "why did (or didn't) this fire, and what would it have
sent?" against a captured review, at an arbitrary wall clock, with no MQTT,
Home Assistant, or Valkey involved. It prints the exact payload each of a
recipient's targets would receive:

```sh
go run ./cmd/replay --at 03:14 \
  --sun dawn=06:12,sunrise=06:42,sunset=19:50,dusk=20:20 \
  internal/frigate/testdata/review-new.json
```

Several files play in order as one review's life, sharing state, so a `new`,
`end` and `genai` message show how each backend's notification is created and
then updated. `review-genai.json` is synthetic (modelled on Frigate's source,
not captured):

```sh
go run ./cmd/replay internal/frigate/testdata/review-{new,end,genai}.json
```

Without `--sun`, solar windows fail open and match everything, which would
quietly make a night rule untestable. Every rule that was rejected reports
which condition rejected it.

## Reviewing what actually happened

`cmd/replay` is hypothetical; `cmd/events` summarizes real reviews and
notifications from the audit store (`internal/eventstore`, best-effort, see
[State](#state)) over a recent window: counts by camera/rule/recipient, delivery
success per recipient, review-to-push latency, and the last N notifications with
their rendered message:

```sh
./scripts/events.sh --since 48h
./scripts/events.sh --recipient bob   # e.g. checking a delivery outage
```

`scripts/db-uri.sh` fills in `DB_URL` from the running config Secret, so neither
script needs a local `config.yaml`. `scripts/mongosh.sh` opens an interactive
shell against a Mongo database (use `psql` for Postgres).
`scripts/notif-log.sh` pretty-prints the request body of every *failed* Home
Assistant call from the pod's logs (the only case `hass.go` logs the payload);
it doesn't touch the database, so it works during an outage even when the audit
store is empty.

Suppressed notifications (cooldown, active-hours, rate-cap) aren't persisted;
they're metrics only (`frigate_notify_suppressed_total`).
