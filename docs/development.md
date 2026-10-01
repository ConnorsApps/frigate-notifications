# Development

## Running locally

```sh
cp config-example.yaml config.yaml
# fill in hass.token, mqtt.password, and media.signingKey (hex)
go run ./cmd/server
```

Set `dryRun: true` to build and log each backend's full payload without
calling any of them.

## Docs

The docs site is built by [Zensical](https://zensical.org) from `mkdocs.yml`
and `docs/`. To preview it:

```sh
pip install zensical
zensical serve   # http://localhost:8000
```

The home and install pages pull their text from `README.md`, between the
snippet markers in its HTML comments, so the README and the site share one
copy: keep those markers when editing the README. `.github/workflows/docs.yml`
builds with `--strict`, which fails on a broken link or anchor, and deploys
`main` to GitHub Pages.

## Schemas

```sh
cd cmd/schema-gen && go run .   # regenerates both schemas below
```

| Schema | For |
|--------|-----|
| `config.schema.json` | Editor validation of `config.yaml` (`# yaml-language-server: $schema=...`) |
| `chart/values.schema.json` | The chart's values, including the whole config under `config`. Helm enforces it on `install`, `upgrade`, `lint` and `template`, so a typo'd key or a bad config fails before anything deploys. |

`cmd/schema-gen` is its own Go module, so the Kubernetes types it reflects
(`k8s.io/api`) stay out of the daemon's `go.mod`. It mirrors `internal/config`
and `chart/values.yaml` by hand: edit `cmd/schema-gen/config.go` or `values.go`
alongside them. The schemas reject unknown keys, so a values key missing from
the generator fails `helm lint`, and CI fails when the checked-in schemas are
stale. Rules spanning several values (Postgres vs Mongo, bundled stores vs
`config.redis.url` / `config.db.url`) live in the chart's `validate` template
for a clearer error.

## Chart

Chart changes need a `version` bump in `chart/Chart.yaml`; the publish job
skips versions that already exist.

Chart tests live in `chart/tests` and run with
[helm-unittest](https://github.com/helm-unittest/helm-unittest):

```sh
helm plugin install https://github.com/helm-unittest/helm-unittest --verify=false
helm unittest chart
```

## Releases

CI publishes the image and the chart to GHCR on every push to `main`; see
[Kubernetes](kubernetes.md) for their references.

The Home Assistant add-on lives in
[ConnorsApps/home-assistant-addons](https://github.com/ConnorsApps/home-assistant-addons).
A `v*` tag also starts that repo's Update workflow, which moves the add-on to the new
image and publishes it. That needs the `HA_ADDONS_TOKEN` secret, a fine-grained
token for home-assistant-addons with Actions: read and write; without it, the
add-on's daily check picks the release up.
