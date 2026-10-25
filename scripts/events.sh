#!/usr/bin/env bash
# Summarise recent frigate-notify activity from the audit store.
# Thin wrapper around ./cmd/events that fills in DB_URL from the cluster. Mongo
# hostnames only resolve in-cluster, so from a laptop use scripts/mongosh.sh.
#
#   ./scripts/events.sh --since 48h --recipient alice
set -euo pipefail

cd "$(dirname "$0")/.."
DB_URL="$(scripts/db-uri.sh)" exec go run ./cmd/events "$@"
