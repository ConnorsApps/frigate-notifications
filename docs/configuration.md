# Configuration

One YAML file sets who gets notified, where, and by which rules.

## Config file

YAML at `CONFIG_PATH` (default `config.yaml`);
[`config-example.yaml`](https://github.com/ConnorsApps/frigate-notifications/blob/main/config-example.yaml)
shows the full shape. Parsing is strict: an unknown key is an error, so a typo
like `sevrity:` can't silently widen a rule.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CONFIG_PATH` | `config.yaml` | Path to the YAML config file |
| `VERSION` | — | Build version, reported to OTel |
| `HASS_*`, `MQTT_*`, `MEDIA_*`, `SLACK_BOT_TOKEN`, `NTFY_*`, `REDIS_URL`, `DB_URL`, `TIMEZONE` | — | Override the matching config key (`HASS_URL` → `hass.url`, `MEDIA_SIGNING_KEY` → `media.signingKey`, …); see the `env` tags in `internal/config/config.go` |

A non-empty variable overrides the file; an empty one is ignored. Settings
without an `env` tag come only from the file.

## Rules

Rules are ordered and the first match wins, so **position is priority**. Each
rule ANDs its `when` conditions; each list within one is an OR; anything
omitted matches everything.

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/ConnorsApps/frigate-notifications/main/config.schema.json
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

### Time windows

`hours` and `activeHours` take `from` and `to`, each a quoted wall-clock
`"HH:MM"`, a 12-hour time (`9am`, `11:30pm`), `noon`/`midnight`, or a solar
event (`sunrise`, `sunset`, `dawn`, `dusk`) with an optional offset
(`dusk+30m`). A `to` earlier than `from` wraps midnight.

Solar events come from Home Assistant's `sun.sun`, so night rules follow the
seasons; a fixed 23:00–06:00 window misses six dark hours in December.

Quote 24-hour times: bare `06:00` is a YAML sexagesimal integer.

### Rule timezone

`hours` use the top-level `timezone` unless a rule sets its own, e.g. for a
camera in another timezone:

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

A list of the same condition blocks as `when`: **any** matching entry
suppresses the rule, and conditions within one entry are ANDed. Use it for "not
when we're home and awake", which sub-label exclusion can't express.

## Recipient policy

Applied after a rule matches, once per recipient rather than per target: a phone
push and a Slack DM are rate-capped as one person, and the digest goes to both.

- `allowCritical: false` downgrades a critical rule to a normal push.
- `activeHours` is the awake window: outside it, non-critical pushes are
  dropped.
- `maxPerHour` collapses the overflow into a self-replacing digest.

Critical notifications bypass active hours and the rate cap; that's what
`allowCritical` opts into. Each recipient's `targets` are covered in
[Backends](notifications.md#backends).
