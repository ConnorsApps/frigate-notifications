# Docker Compose

Three files in one folder: addresses and tokens in `.env`, people and rules in
`config.yaml`, and `compose.yaml` to run it with Valkey and PostgreSQL.

--8<-- "README.md:compose"

=== ".env"

    ```sh
    --8<-- "compose/.env.example"
    ```

=== "config.yaml"

    ```yaml
    --8<-- "compose/config-example.yaml"
    ```

=== "compose.yaml"

    ```yaml
    --8<-- "compose/compose.yaml"
    ```

Frigate's own Compose file doesn't publish port 5000. Publish it, or add these
services to that file and set `MEDIA_FRIGATE_URL=http://frigate:5000`.
