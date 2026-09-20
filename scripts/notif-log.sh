#!/usr/bin/env bash
# Pretty-print the request body of every FAILED Home Assistant notify call from
# the pod's logs (hass.go logs `body=` only on the error path). Doesn't touch
# the database, so it works even when the audit store is empty.
#
#   ./scripts/notif-log.sh 500
set -euo pipefail

kubectl -n "${NS:-frigate}" logs deploy/frigate-notify --tail "${1:-200}" \
	| sed -r 's/\x1b\[[0-9;]*m//g' \
	| grep -oE 'body=\{.*\}' \
	| sed 's/^body=//' \
	| jq .
