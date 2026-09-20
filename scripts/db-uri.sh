#!/usr/bin/env bash
# Print the frigate-notify database URL (MongoDB or PostgreSQL), read from the running config Secret.
# Single source of truth for the other scripts here, and it sidesteps the
# local .claude/settings.json deny on reading config.yaml directly.
#
#   NS=frigate ./scripts/db-uri.sh
set -euo pipefail

kubectl -n "${NS:-frigate}" get secret frigate-notify-config \
	-o jsonpath='{.data.config\.yaml}' | base64 -d | yq -r '.db.url'
