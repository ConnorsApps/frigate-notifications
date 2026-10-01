# Frigate Notifications

<!-- --8<-- [start:pitch] -->
Rule-based [Frigate](https://frigate.video) alerts for the whole household, on
the Home Assistant Companion app, Slack, ntfy, and Discord.

## Why use it

- **Wake me for a stranger, not the family.** A person after dusk who isn't a
  recognized face, with the alarm armed, becomes a critical alert that gets
  through Do Not Disturb on the Companion app.
- **One set of rules for the whole house.** Rules name people, and each person
  keeps their own awake hours, critical opt-in, and hourly cap.
- **Everyone on the app they already use.** One person can get the Companion
  app and a Slack DM, another just ntfy or Discord.
- **A snapshot now, the clip when it's over.** When the event ends, the same
  notification quietly updates with the clip, an animated preview, and
  Frigate's GenAI summary.
- **Pictures that load anywhere.** Media goes through signed, expiring links,
  so phones away from home need no VPN or Frigate login.
- **No alert storms.** Cooldowns, plus a single digest for anything past a
  person's hourly cap.
- **"Why did that fire?"** Replay a Frigate review against your rules, or send
  a real test notification to your own phone.

## What a rule looks like

```yaml
rules:                                 # the first match wins
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

Coming from [SgtBatten's Frigate blueprint](https://github.com/SgtBatten/HA_blueprints)?
It takes an automation per person; here one rule list covers everyone.
<!-- --8<-- [end:pitch] -->

## Install

<!-- --8<-- [start:requirements] -->
You need Frigate publishing to an MQTT broker, and Home Assistant, even if you
only notify Slack or Discord: rules read its state (the sun, your alarm). For
pictures on phones away from home, put a public HTTPS URL (a reverse proxy or
tunnel) in front of port 8081.

| Method | Pick it if | Comes with |
|---|---|---|
| Home Assistant add-on | You run Home Assistant OS or Supervised | Home Assistant, Mosquitto, and a signing key, set up for you |
| Docker Compose | You have any Docker host | Valkey and PostgreSQL |
| Kubernetes (Helm) | You run a cluster | Optional Valkey, PostgreSQL, or MongoDB; a Gateway API route; OpenTelemetry |
<!-- --8<-- [end:requirements] -->

### Home Assistant add-on

<!-- --8<-- [start:ha] -->
[![Add the repository to your Home Assistant](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2FConnorsApps%2Fhome-assistant-addons)

Add the repository, install **Frigate Notifications**, and start it once: it
writes an example `config.yaml` to
`/addon_configs/38cd5911_frigate_notifications/` and stops. Fill in your
recipients, cameras, and rules, then start it again.
<!-- --8<-- [end:ha] -->

[Add-on guide](https://github.com/ConnorsApps/home-assistant-addons/blob/main/frigate-notifications/DOCS.md)

### Docker Compose

<!-- --8<-- [start:compose] -->
```sh
url=https://raw.githubusercontent.com/ConnorsApps/frigate-notifications/main
curl -O "$url/docker-compose.yml"
curl -o config.yaml "$url/config-example.yaml"
# fill in config.yaml: Home Assistant, MQTT, media, recipients, cameras, rules
docker compose up -d
```
<!-- --8<-- [end:compose] -->

[Docker Compose guide](https://connorsapps.github.io/frigate-notifications/docker-compose/)

### Kubernetes (Helm)

<!-- --8<-- [start:helm] -->
```sh
helm upgrade --install frigate-notifications \
  oci://ghcr.io/connorsapps/charts/frigate-notifications \
  --values values.yaml   # your config.yaml under the `config` key
```
<!-- --8<-- [end:helm] -->

[Kubernetes guide](https://connorsapps.github.io/frigate-notifications/kubernetes/)

## Documentation

The full docs live at
**[connorsapps.github.io/frigate-notifications](https://connorsapps.github.io/frigate-notifications/)**:
configuration, notification backends, how it works, troubleshooting, and
development.
