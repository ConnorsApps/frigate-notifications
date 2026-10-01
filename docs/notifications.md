# Notifications

Which apps a notification reaches, what it carries, and how it changes over
a review's life.

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
| `hass` | still as `image` (Android, animated on 14+); the clip, else the still, as an `attachment` (iOS) | "View Clip" action | iOS critical alert, Android `alarm_stream` channel | replaces by `tag`; Android `alert_once`, iOS passive, no sound |
| `ntfy` | snapshot as `attach` | "View Clip" action | priority 5, 🚨 tag | replaces by `sequence_id`, priority 2 |
| `slack` | image block | link in the context line | 🚨 in the header | `chat.update` edits the message |
| `discord` | embed image | link in the embed | red embed, 🚨 in the headline | edits the message |

The layout follows the event. Every alert carries the camera, what was detected,
where (zones), when, and severity; an ended review adds how long it lasted and
the clip; a GenAI summary replaces the headline and adds its sentence. Each
backend lays that out for itself:

- **`hass`**: grouped per camera, "Alert · Zone" as the iOS subtitle, the review
  start as the Android timestamp. Alerts are time-sensitive on iOS so they get
  through a Focus mode; detections are not. A digest uses its own low-importance
  channel. Android shows no picture when the Companion app's only Home Assistant URL is
  `http://` and the phone is away from home, unless it allows insecure
  connections.
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
does not replace: an update arrives as a second, passive notification. It
attaches the snapshot, not the gif: Android auto-downloads only up to 1 MB by
default, and a long review's gif is bigger. A self-hosted server needs
`upstream-base-url: https://ntfy.sh` for instant delivery to iOS. Authenticate with an access token (`tk_...`, sent as a Bearer
token) for a write-only user on the topic, with `auth-default-access: deny-all`
on the server. The sequence id is derived from the review id with anything
outside `A-Z a-z 0-9 - _` replaced, because ntfy rejects the "." in Frigate's
ids.

**Discord.** The webhook URL is the whole credential: treat it like a token. It
is never logged, traced, or written to the audit log at runtime (only its id is).

**Reachability and privacy.** Slack and Discord fetch snapshots from the
[media proxy](how-it-works.md#media-proxy) themselves, so `media.publicBaseURL` has to be
reachable from their servers, not just from phones. Both services also cache
what they fetch, so a snapshot posted to a channel outlives its signed link and
is visible to everyone in that channel. Point rules at shared channels
accordingly.

Text written by the model (GenAI titles, summaries, descriptions) is escaped for
Slack and Discord markup, formatting marks are defused, and Discord mentions are
disabled, so a description can't ping a channel or restyle the message.

## Media and presets

Media is picked automatically. Each candidate is probed against Frigate
in-cluster, and the first one that exists and fits wins; if none does, the push
goes out as text. The clip, the best still (preview gif, else snapshot) and the
snapshot are resolved independently, and each backend shows what it can: the
Home Assistant Companion app plays the clip on iOS and shows the still on Android (which
shows only a few frames of a video), while Slack, Discord, and ntfy can't play a
clip inline, so they get a still and a "View Clip" link; see
[Backends](#backends).

| phase | order tried |
|---|---|
| `new` | snapshot → text |
| `end` | clip (≤25 MB), then the first still that fits: review preview gif (≤10 MB) → snapshot; text if neither |

The clip spans the whole review (`/api/<camera>/start/<s>/end/<e>/clip.mp4`, 2s
before and 3s after), not just its first detection. The clip cap is below iOS's
50 MB hard limit because the notification extension has ~30 s to download it on
whatever connection the phone has. A clip too big to attach is still linked from
"View Clip", which opens a [player page](how-it-works.md#media-proxy), not the mp4.

H.265 cameras need Frigate's `ffmpeg: {apple_compatibility: true}`, or their
clips are tagged `hev1`, which iOS won't play (`notify-test --check-media`
reports the tag).

`preset` is optional and only overrides that:

| preset | effect |
|---|---|
| `auto` (default) | the selection above |
| `text` | no attachment (the "View Clip" action is still offered) |
| `liveview` | `auto`, plus the camera's `liveViewEntity` for an iOS live stream on expand, until the clip exists (iOS would show the camera over it) |

`frigate_notify_media_selected_total{phase,kind}` counts what each notification
carried; `kind="none"` means every candidate failed and it fell back to text
(`off` is a rule using `preset: text`). Check it first when someone
says the images stopped showing up.

`critical: true` emits both iOS (`push.interruption-level`) and Android
(`alarm_stream` channel, `ttl`/`priority`) keys in one payload; each platform
ignores the other's.

## Lifecycle

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
backend in the [Backends](#backends) table). A critical review's update drops the
critical push, because iOS can't replace a critical notification and would ring
twice.
