#!/usr/bin/env bash
# Interactive mongosh against the frigate-notify database, run inside a mongod
# pod so in-cluster hostnames resolve. MongoDB db.url only; credentials come
# from the config Secret via db-uri.sh.
#
#   MONGO_NS=shared-mongo MONGO_POD=shared-mongo-0 ./scripts/mongosh.sh [--eval '...']
set -euo pipefail

uri="$("$(dirname "$0")/db-uri.sh")"
creds="${uri#mongodb://}"
creds="${creds%%@*}"
user="${creds%%:*}"
pass="${creds#*:}"

exec kubectl -n "${MONGO_NS:-shared-mongo}" exec -it "${MONGO_POD:-shared-mongo-0}" -c mongod -- \
	mongosh --quiet -u "$user" -p "$pass" \
	--authenticationDatabase frigate-notify \
	localhost:27017/frigate-notify "$@"
