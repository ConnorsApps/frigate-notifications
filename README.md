# Frigate Notifications

<!-- --8<-- [start:pitch] -->
Rule-based [Frigate](https://frigate.video) alerts for the whole household, on
the Home Assistant Companion app, Slack, ntfy, and Discord.

## Why use it

- **Wake me for a stranger, not the family.** A person after dusk, no known
  face, alarm armed: a critical alert through Do Not Disturb (Companion app).
- **One rule list for everyone.** Each person picks their apps and keeps their
  own awake hours, critical opt-in, and hourly cap.
- **Snapshot now, clip when it ends.** The same notification quietly updates
  with the clip and Frigate's GenAI summary.
- **Pictures that load anywhere.** Signed, expiring links: no VPN or Frigate
  login.
- **No alert storms.** Cooldowns, and a digest past each person's hourly cap.
- **"Why did that fire?"** Replay a review against your rules, or send yourself
  a test.

## What a rule looks like

```yaml
rules:                                 # first match wins
  - name: night-person
    when:
      hours: { from: dusk+30m, to: dawn-30m }
      labels: [person]
      excludeSubLabels: [alice, bob]   # recognized faces
    unless:
      - entityState: { alarm_control_panel.home: disarmed }
    to: [alice, bob]
    critical: true
```

Coming from [SgtBatten's blueprint](https://github.com/SgtBatten/HA_blueprints)?
One rule list replaces its automation per person.
<!-- --8<-- [end:pitch] -->

## Install

<!-- --8<-- [start:requirements] -->
Needs Frigate on MQTT, and Home Assistant: rules read its state, even if you
only notify Slack. For pictures away from home, serve port 8081 at a public
HTTPS URL.

| | Runs on | Includes |
|---|---|---|
| **Home Assistant add-on** | Home Assistant OS or Supervised | Home Assistant and Mosquitto connected, signing key generated |
| **Docker Compose** | Any Docker host | Valkey, PostgreSQL |
| **Kubernetes (Helm)** | Any cluster | Optional Valkey, PostgreSQL or MongoDB; Gateway API route; OpenTelemetry |
<!-- --8<-- [end:requirements] -->

### Home Assistant add-on

<!-- --8<-- [start:ha] -->
[![Add the repository to your Home Assistant](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2FConnorsApps%2Fhome-assistant-addons)

1. Add the repository and install **Frigate Notifications**.
2. Start it once to generate
   `/addon_configs/38cd5911_frigate_notifications/config.yaml`.
3. Fill in recipients, cameras, and rules, then start it again.
<!-- --8<-- [end:ha] -->

[Add-on guide](https://github.com/ConnorsApps/home-assistant-addons/blob/main/frigate-notifications/DOCS.md)

### Docker Compose

<!-- --8<-- [start:compose] -->
```sh
url=https://raw.githubusercontent.com/ConnorsApps/frigate-notifications/main
curl -O "$url/docker-compose.yml"
curl -o config.yaml "$url/config-example.yaml"   # then edit it
docker compose up -d
```
<!-- --8<-- [end:compose] -->

[Docker Compose guide](https://connorsapps.github.io/frigate-notifications/docker-compose/)

### Kubernetes (Helm)

<!-- --8<-- [start:helm] -->
```sh
helm upgrade --install frigate-notifications \
  oci://ghcr.io/connorsapps/charts/frigate-notifications \
  --values values.yaml   # config.yaml under `config:`
```
<!-- --8<-- [end:helm] -->

[Kubernetes guide](https://connorsapps.github.io/frigate-notifications/kubernetes/)

## Documentation

[connorsapps.github.io/frigate-notifications](https://connorsapps.github.io/frigate-notifications/)
