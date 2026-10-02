# Docker Compose

[`docker-compose.yml`](https://github.com/ConnorsApps/frigate-notifications/blob/main/docker-compose.yml)
runs the image with Valkey and PostgreSQL:

--8<-- "README.md:compose"

- Compose sets `REDIS_URL` and `DB_URL`, overriding `redis.url` and `db.url`.
- Set `POSTGRES_PASSWORD` in the environment or an `.env` file.
- `media.signingKey` is hex, at least 32 characters: `openssl rand -hex 32`.
- Only the media port, 8081, is published. Put a reverse proxy in front of it
  and set `media.publicBaseURL` to its URL.
- `mqtt.broker` and `media.frigateURL` must be reachable from the container, so
  not `localhost`.
- Pin a release by replacing `:latest` with a version, e.g. `:0.2.0`.

Any secret can come from the environment instead of `config.yaml`; see
[Environment variables](configuration.md#environment-variables).
