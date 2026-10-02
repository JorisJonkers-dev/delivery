#!/usr/bin/env bash
# Vendors deploy-kit's published JSON Schemas at one commit, with the accept/refuse corpus each
# ships with and every spec file a corpus case reads, into DEST. Paths under DEST mirror spec/v1/.
#
#   scripts/sync-schemas.sh REF DEST SCHEMA...
set -euo pipefail

ref="$1" dest="$2"
shift 2
base="https://raw.githubusercontent.com/JorisJonkers-dev/deploy-kit/${ref}/spec/v1"

fetch() {
	mkdir -p "$(dirname "${dest}/$1")"
	curl -fsSL --retry 3 "${base}/$1" -o "${dest}/$1"
}

rm -rf "${dest}"
for schema in "$@"; do
	fetch "schemas/${schema}.schema.json"
	fetch "schemas/corpus/${schema}.corpus.json"
	jq -r '.cases[].file // empty' "${dest}/schemas/corpus/${schema}.corpus.json" | sort -u |
		while read -r file; do fetch "${file}"; done
done
printf '%s\n' "${ref}" >"${dest}/REF"
