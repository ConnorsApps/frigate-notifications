# Development

## Running locally

```sh
cp config-example.yaml config.yaml
# fill in hass.token, mqtt.password, and media.signingKey (hex)
go run ./cmd/server
```

`dryRun: true` builds and logs each backend's full payload without calling any
of them.

## Docs

```sh
pip install zensical
zensical serve   # http://localhost:8000
```

The site builds from `mkdocs.yml` and `docs/`. Its home and install pages
include sections of `README.md`, so keep the README's snippet-marker comments.
CI builds with `--strict`, so a broken link or anchor fails, and deploys `main`
to GitHub Pages.

## Schemas

`cd cmd/schema-gen && go run .` regenerates `config.schema.json` (editor
validation) and `chart/values.schema.json` (enforced by Helm). The generator is
its own Go module, keeping `k8s.io/api` out of the daemon's `go.mod`. It mirrors
`internal/config` and `chart/values.yaml` by hand: edit
`cmd/schema-gen/config.go` or `values.go` alongside them. The schemas reject
unknown keys, so a values key missing from the generator fails `helm lint`, and
CI fails when the checked-in schemas are stale. Rules spanning several values
(Postgres vs Mongo, bundled stores vs `config.redis.url` / `config.db.url`) live
in the chart's `validate` template.

## Chart

Chart changes need a `version` bump in `chart/Chart.yaml`; the publish job
skips versions that already exist. Tests live in `chart/tests` and run with
[helm-unittest](https://github.com/helm-unittest/helm-unittest):

```sh
helm plugin install https://github.com/helm-unittest/helm-unittest --verify=false
helm unittest chart
```

## Releases

Every push to `main` publishes `ghcr.io/connorsapps/frigate-notifications`
(`latest`, `sha-<short>`) and, when `chart/` changed, the chart to
`oci://ghcr.io/connorsapps/charts/frigate-notifications`. A `v*` tag adds
`<version>` and `<major>.<minor>` image tags and starts the Update workflow of
the [Home Assistant add-on](https://github.com/ConnorsApps/home-assistant-addons).
That needs the `HA_ADDONS_TOKEN` secret, a fine-grained token for
home-assistant-addons with Actions: read and write; without it, the add-on's
daily check picks the release up.
