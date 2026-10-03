# Kubernetes (Helm)

--8<-- "README.md:helm"

```yaml title="values.yaml"
# yaml-language-server: $schema=https://raw.githubusercontent.com/ConnorsApps/frigate-notifications/main/chart/values.schema.json
valkey:
  enabled: true # keeps state across restarts
config: # config.yaml, rendered into a Secret: keep this file out of git
  hass:
    url: http://home-assistant.home-assistant.svc:8123
    token: "" # a long-lived access token
  mqtt:
    broker: tcp://mosquitto.mosquitto.svc:1883
    username: frigate-notify
    password: ""
  media:
    frigateURL: http://frigate.frigate.svc:5000
    publicBaseURL: https://frigate-notifications.example.com
    signingKey: "" # openssl rand -hex 32
  timezone: America/New_York
  recipients:
    alice:
      targets: [{ type: hass, service: mobile_app_alices_phone }]
  cameras:
    front_door: { friendlyName: Front Door }
  rules:
    - name: person
      when: { labels: [person] }
      to: [alice]
```

Pin the chart with `--version`; `image.tag` defaults to the chart's `appVersion`.

## Exposing media

`httpRoute` renders a Gateway API route to the media port, for paths under
`pathPrefix` (default `/m/`, where every signed link lives):

```yaml
httpRoute:
  enabled: true
  hostnames: [frigate-notifications.example.com] # the host of media.publicBaseURL
  parentRefs:
    - { name: public, namespace: gateways, sectionName: https }
```

## Bundled backing services

| Values | Deploys | Prerequisite |
|--------|---------|--------------|
| `valkey.enabled` | Valkey `StatefulSet` (no auth, cluster-internal) | none |
| `postgres.enabled` | CloudNativePG `Cluster` | [CloudNativePG](https://cloudnative-pg.io) operator |
| `mongo.enabled` | Official `mongo` image `StatefulSet` | none |
| `mongo.enabled` + `mongo.mode: operator` | `MongoDBCommunity` resource | MongoDB Controllers for Kubernetes operator (`mongodb/mongodb-kubernetes`) |

- Each sets `REDIS_URL` / `DB_URL`, so leave `config.redis.url` /
  `config.db.url` unset. Enable at most one of `postgres` / `mongo`.
- `extraSpec` is deep-merged over the rendered spec (maps merge, lists
  replace), so any field can be set, e.g.
  `postgres.extraSpec.postgresql.parameters.max_connections`.
- `scripts/db-uri.sh` reads `db.url` from the config Secret, so it can't find a
  bundled database.

## Telemetry

Off until an endpoint is set; `otel` renders the standard `OTEL_*` variables.

```yaml
otel:
  # Both signals; over HTTP, /v1/traces and /v1/metrics are appended.
  endpoint: http://collector.example.svc:4318
  # Or per signal, used verbatim:
  metrics: # OTLP/HTTP only
    endpoint: http://collector.example.svc:8480/insert/0/opentelemetry/v1/metrics
  traces:
    endpoint: http://collector.example.svc:4317
    protocol: grpc
    sampler: parentbased_traceidratio
    samplerArg: "0.1" # 10%
```

Put secret headers in `extraEnv` with `valueFrom`, not `otel.headers`;
`extraEnv` also overrides any generated variable.
