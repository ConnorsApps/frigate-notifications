# Configuration

Everything is set in one YAML file: who gets notified, on which apps, and
by which rules.

## Config file

YAML at `CONFIG_PATH` (default `config.yaml`). Parsing is strict: an unknown
key is an error, because a typo'd condition (`sevrity:`) would otherwise
silently widen a rule to match everything.

See [`config-example.yaml`](https://github.com/ConnorsApps/frigate-notifications/blob/main/config-example.yaml) for the full shape.
[`config.schema.json`](https://github.com/ConnorsApps/frigate-notifications/blob/main/config.schema.json) gives editors validation of
`config.yaml` (`# yaml-language-server: $schema=...`).

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CONFIG_PATH` | `config.yaml` | Path to the YAML config file |
| `VERSION` | — | Build version, reported to OTel |
| `HASS_*`, `MQTT_*`, `MEDIA_*`, `SLACK_BOT_TOKEN`, `NTFY_*`, `REDIS_URL`, `DB_URL`, `TIMEZONE` | — | Override the matching config key (`HASS_URL` → `hass.url`, `MEDIA_SIGNING_KEY` → `media.signingKey`, …); see the `env` tags in `internal/config/config.go` |

A non-empty variable beats the file and an empty one is ignored, so the chart and
the Home Assistant add-on can inject secrets. A required value must come from one or
the other. Settings without an `env` tag come only from the file.

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

## Recipient policy

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

A recipient's `targets`, and what each backend needs, are covered in
[Backends](notifications.md#backends).
