# Kubernetes (Helm)

The Helm chart runs the service, and can deploy its state and audit stores
alongside it.

CI publishes both artifacts to GHCR on every push to `main`:

| Artifact | Reference |
|----------|-----------|
| Image | `ghcr.io/connorsapps/frigate-notifications` (`latest`, `<version>` from `v*` tags, `sha-<short>`) |
| Helm chart | `oci://ghcr.io/connorsapps/charts/frigate-notifications` |

## Install

```sh
helm upgrade --install frigate-notifications oci://ghcr.io/connorsapps/charts/frigate-notifications \
  --version <chart version> --set image.tag=<version> \
  --values values.yaml   # config: <the full config.yaml under the `config` key>
```

The chart renders `config` into a Secret mounted at
`/etc/frigate-notify/config.yaml`. Keep the real config (tokens, signing key,
URLs) out of git.

The chart's values schema covers the whole config under `config`. Helm
enforces it on `install`, `upgrade`, `lint` and `template`, so a typo'd key or a
bad config fails before anything deploys.

## Exposing media

The chart can render the route itself. `httpRoute` always targets the media
port, and only paths under `pathPrefix` (default `/m/`, where every signed link
lives) are routed:

```yaml
httpRoute:
  enabled: true
  hostnames: [frigate-notifications.example.com]
  parentRefs:
    - {name: public, namespace: gateways, sectionName: https}
```

Only the media port is ever routed; see [Media proxy](how-it-works.md#media-proxy)
for why health checks stay internal.

## Bundled backing services

The chart can deploy the state and audit stores for you. All are off by
default; when enabled the chart sets `REDIS_URL` / `DB_URL`, so leave
`config.redis.url` / `config.db.url` unset (the render fails if both are
given).

| Values | Deploys | Prerequisite |
|--------|---------|--------------|
| `valkey.enabled` | Valkey `StatefulSet` (no auth, cluster-internal) | none |
| `postgres.enabled` | CloudNativePG `Cluster` | [CloudNativePG](https://cloudnative-pg.io) operator |
| `mongo.enabled` | Official `mongo` image `StatefulSet` | none |
| `mongo.enabled` + `mongo.mode: operator` | `MongoDBCommunity` resource | MongoDB Controllers for Kubernetes operator (`mongodb/mongodb-kubernetes`) |

Each block also takes `extraSpec`, deep-merged over the rendered resource's
spec (maps merge, lists replace), plus `extraEnv` (and `valkey.extraArgs`), so
any operator or StatefulSet field can be set from values without forking, e.g.
`postgres.extraSpec.postgresql.parameters.max_connections`.

Only one of `postgres` / `mongo` (the app has a single `db.url`). State follows
the usual precedence: Valkey, else the database, else memory. The chart does
not install operators. `scripts/db-uri.sh` reads `db.url` from the config
Secret, so it has nothing to read when the URL comes from a bundled service.

## Telemetry

Traces and metrics are exported over OTLP only when an endpoint is configured;
otherwise the app exports nothing. The chart's `otel` values render the
standard `OTEL_*` environment variables (mapping in `chart/values.yaml`):

```yaml
# chart values
otel:
  endpoint: http://collector.example.svc:4318   # base URL for both signals
```

Over HTTP the SDK appends `/v1/traces` and `/v1/metrics` to the base endpoint.
For a backend that serves other paths, or to split the signals, use the
per-signal endpoints, which are used verbatim:

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

Metrics are OTLP/HTTP only; traces may use `grpc`. Put secret header values in
`extraEnv` with `valueFrom` rather than `otel.headers`. Anything in `extraEnv`
replaces the generated variable of the same name, and any other `OTEL_*`
variable the SDK understands can be set there too.
