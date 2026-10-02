#!/usr/bin/env bash
# Vendors deploy-kit's published JSON Schemas at one commit, with the accept/refuse corpus each
# ships with and every spec file a corpus case reads, into DEST. Paths under DEST mirror spec/v1/.
# Everything is fetched beside DEST first, so a failed fetch leaves DEST as it was.
#
#   scripts/sync-schemas.sh REF DEST SCHEMA...
set -euo pipefail

ref="$1" dest="$2"
shift 2
base="https://raw.githubusercontent.com/JorisJonkers-dev/deploy-kit/${ref}/spec/v1"

staging="$(mktemp -d "${dest%/}.XXXXXX")"
trap 'rm -rf "${staging}"' EXIT

fetch() {
	mkdir -p "$(dirname "${staging}/$1")"
	curl -fsSL --retry 3 "${base}/$1" -o "${staging}/$1"
}

for schema in "$@"; do
	fetch "schemas/${schema}.schema.json"
	fetch "schemas/corpus/${schema}.corpus.json"
	jq -r '.cases[].file // empty' "${staging}/schemas/corpus/${schema}.corpus.json" | sort -u |
		while read -r file; do fetch "${file}"; done
done
printf '%s\n' "${ref}" >"${staging}/REF"
rm -rf "${dest}"
mv "${staging}" "${dest}"
