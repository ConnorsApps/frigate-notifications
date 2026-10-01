# Docker Compose

[`docker-compose.yml`](https://github.com/ConnorsApps/frigate-notifications/blob/main/docker-compose.yml)
runs the published image alongside Valkey and PostgreSQL:

--8<-- "README.md:compose"

Compose sets `REDIS_URL` and `DB_URL`, which override `redis.url` and `db.url`
in `config.yaml`. Set `POSTGRES_PASSWORD` in the environment or an `.env` file.
`media.signingKey` is hex, at least 32 characters; `openssl rand -hex 32` makes
one.

Only the media port (`8081`) is published: put a reverse proxy in front of it
and point `media.publicBaseURL` there. `mqtt.broker` and `media.frigateURL` must
be reachable from the container, so not `localhost`.

The image is `ghcr.io/connorsapps/frigate-notifications`, tagged `latest`,
`<version>` from `v*` tags, and `sha-<short>`. Secrets can come from the
environment instead of `config.yaml`; see
[Environment variables](configuration.md#environment-variables).
