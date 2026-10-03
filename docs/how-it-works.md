# How it works

```mermaid
flowchart TD
  frigate[Frigate] -->|reviews, over MQTT| app[Frigate Notifications]
  hass[Home Assistant] -->|states, sun| app
  app -->|pushes, via Home Assistant| companion[Companion app]
  app --> chat[Slack, ntfy, Discord]
```

## Deciding

```mermaid
flowchart TD
  review[New review] --> rule{{First matching rule?}}
  rule -->|none| drop1([Nothing sent])
  rule -->|match| cooldown{{Rule cooling down?}}
  cooldown -->|yes| drop2([Nothing sent])
  cooldown -->|no| each
  subgraph each [For each of the rule's recipients]
    direction TB
    critical{{Critical, and allowCritical?}} -->|yes| send([Sent to every target])
    critical -->|no| awake{{Inside activeHours?}}
    awake -->|no| drop3([Nothing sent])
    awake -->|yes| cap{{Past maxPerHour?}}
    cap -->|no| send
    cap -->|yes| digest([Folded into a digest])
  end
```

## A review's life

One notification per review, updated in place without alerting again:

```mermaid
---
config:
  sequence:
    mirrorActors: false
---
sequenceDiagram
  participant F as Frigate
  participant N as Frigate Notifications
  participant P as Phone
  F->>N: new
  Note over N: wait out any holdoff, then decide
  N->>P: alert, with the snapshot
  F->>N: end
  N-->>P: quiet update, with the clip
  F->>N: genai
  N-->>P: quiet update, with the summary
```

A later phase alerts again only to raise a review to critical; see
[Lifecycle](notifications.md#lifecycle).

## Media proxy

Notifications can't carry a Frigate login, so their links point at this
service's media port, which fetches from Frigate's unauthenticated API:

```mermaid
---
config:
  sequence:
    mirrorActors: false
---
sequenceDiagram
  participant P as Phone, Slack, Discord
  participant N as Frigate Notifications :8081
  participant F as Frigate :5000
  P->>N: GET /m/clip/ID.mp4?exp=…&sig=…
  Note over N: check signature and expiry
  N->>F: GET the clip
  F-->>N: clip
  N-->>P: clip
```

- Links are HMAC-signed and expire after `media.linkTTL` (default 24h).
  Rotating `media.signingKey` revokes them all.
- "View Clip" opens a player page: Frigate's HLS where the browser plays it,
  else the mp4.
- Health checks are on :8080, which is never routed publicly.

## State

Cooldowns, review state, rate caps and digests are kept in:

```mermaid
flowchart LR
  redis{redis.url?} -->|set| valkey[(Valkey)]
  redis -->|unset| db{db.url?}
  db -->|set| database[(PostgreSQL or MongoDB)]
  db -->|unset| memory[Memory, lost on restart]
```

Every store operation fails open: on an error it falls back to memory, so an
outage costs an extra notification, never a missed one. `db.url` also keeps a
year of history, every review and delivery attempt, for
[`cmd/events`](troubleshooting.md#reviewing-what-actually-happened).
