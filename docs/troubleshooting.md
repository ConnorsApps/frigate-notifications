# Troubleshooting

Three tools, each for a different question: did it show up (`notify-test`), why
did it fire (`replay`), and what actually happened (`events`). Run them from a
checkout of this repo, with Go installed.

## Sending a test notification

`cmd/notify-test` sends one real notification to a recipient's targets, or to one
backend with `--target slack|ntfy|discord|hass`. It answers what replay can't:
whether the message actually shows up, image and all. `--update-after 10s` then
sends the end-of-review update to the same message, which is how to check on
your own devices that each backend edits in place and stays quiet.

`--review <id>` builds both from a real Frigate review, with production media
and links. `--check-media` instead fetches every link as a phone would, player
page and HLS included, and reports status, type, size and codec. Frigate must be
reachable:

```sh
kubectl -n frigate port-forward svc/frigate 5000 &
MEDIA_FRIGATE_URL=http://localhost:5000 go run ./cmd/notify-test --review <id> --check-media
MEDIA_FRIGATE_URL=http://localhost:5000 go run ./cmd/notify-test --review <id> --update-after 30s
```

What each device should show:

| device | first push | end-of-review update |
|---|---|---|
| iPhone (Home Assistant) | snapshot thumbnail; `liveview` expands to the live camera | replaces it silently; expanding plays the clip; "View Clip" plays in Safari |
| Android 14+ (Home Assistant) | snapshot as a big picture | replaces it silently; the gif animates when expanded |
| Android 13 and older | snapshot | the gif, as a still |
| ntfy Android / iOS | snapshot | replaces it quietly / a second, passive notification |
| Slack, Discord | snapshot | the message is edited to the animated gif; "View clip" plays |

Android's `alert_once` applies only while the notification is showing, so an
update to one already swiped away alerts again.

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
[State](how-it-works.md#state)) over a recent window: counts by camera/rule/recipient, delivery
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

## Ops scripts

`scripts/` holds Kubernetes ops helpers (`kubectl`/`mongosh` against a running
deployment). They default to namespace `frigate`; override with `NS`,
`MONGO_NS`, and `MONGO_POD` (the last two only apply to `mongosh.sh`, which
needs a MongoDB `db.url`).
