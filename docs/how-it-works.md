# How it works

```mermaid
flowchart TD
  frigate[Frigate] -->|reviews, over MQTT| app[Frigate Notifications]
  hass[Home Assistant] -->|states, sun| app
  app -->|pushes, via Home Assistant| companion[Companion app]
  app --> chat[Slack, ntfy, Discord]
```

Rules and each person's [recipient policy](configuration.md#recipient-policy)
decide who's notified; the notification then follows the review's
[lifecycle](notifications.md#lifecycle).

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
  redis{{redis.url?}} -->|set| valkey[(Valkey)]
  redis -->|unset| db{{db.url?}}
  db -->|set| database[(PostgreSQL or MongoDB)]
  db -->|unset| memory[Memory, lost on restart]
```

Every store operation fails open: on an error it falls back to memory, so an
outage costs an extra notification, never a missed one. `db.url` also keeps a
year of history, every review and delivery attempt, for
[`cmd/events`](troubleshooting.md#reviewing-what-actually-happened).
