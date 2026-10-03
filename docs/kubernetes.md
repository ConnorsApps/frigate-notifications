# Kubernetes (Helm)

--8<-- "README.md:helm"

The chart renders `config` into a Secret mounted at
`/etc/frigate-notify/config.yaml`; keep the real values out of git. Its schema
covers the whole config, so a typo'd key fails before anything deploys. Pin the
chart with `--version`; `image.tag` defaults to the chart's `appVersion`.

## Exposing media

`httpRoute` renders a Gateway API route to the media port, for paths under
`pathPrefix` (default `/m/`, where every signed link lives):

```yaml
httpRoute:
  enabled: true
  hostnames: [frigate-notifications.example.com]
  parentRefs:
    - {name: public, namespace: gateways, sectionName: https}
```

## Bundled backing services

Off by default. When enabled, the chart sets `REDIS_URL` / `DB_URL`, so leave
`config.redis.url` / `config.db.url` unset (rendering fails if both are set).

| Values | Deploys | Prerequisite |
|--------|---------|--------------|
| `valkey.enabled` | Valkey `StatefulSet` (no auth, cluster-internal) | none |
| `postgres.enabled` | CloudNativePG `Cluster` | [CloudNativePG](https://cloudnative-pg.io) operator |
| `mongo.enabled` | Official `mongo` image `StatefulSet` | none |
| `mongo.enabled` + `mongo.mode: operator` | `MongoDBCommunity` resource | MongoDB Controllers for Kubernetes operator (`mongodb/mongodb-kubernetes`) |

Each block takes `extraSpec`, deep-merged over the rendered resource's spec
(maps merge, lists replace), plus `extraEnv` (and `valkey.extraArgs`), so any
operator or StatefulSet field can be set without forking, e.g.
`postgres.extraSpec.postgresql.parameters.max_connections`.

Enable at most one of `postgres` / `mongo`. The chart doesn't install
operators. `scripts/db-uri.sh` reads `db.url` from the config Secret, so it
can't find a bundled database.

## Telemetry

Off until an endpoint is set. The chart's `otel` values render the standard
`OTEL_*` variables (mapping in `chart/values.yaml`):

```yaml
otel:
  endpoint: http://collector.example.svc:4318   # base URL for both signals
```

Over HTTP the SDK appends `/v1/traces` and `/v1/metrics` to the base endpoint.
For other paths, or to split the signals, set the per-signal endpoints, which
are used verbatim:

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

Metrics are OTLP/HTTP only; traces may use `grpc`. Put secret headers in
`extraEnv` with `valueFrom`, not `otel.headers`. `extraEnv` replaces any
generated variable of the same name and can set any other `OTEL_*` variable.
