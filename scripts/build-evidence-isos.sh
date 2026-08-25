#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Build and read back the reserve ISO master described in docs/EVIDENCE.md.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
EVIDENCE_DIR=${BORGE_EVIDENCE_DIR:-/home/renes/evidence/borge}
ISO_DIR=${BORGE_EVIDENCE_ISO_DIR:-/media/renes/SEAGATE2TB/borge-evidence-isos}
MANIFEST=${BORGE_EVIDENCE_MANIFEST:-$ROOT/evidence/manifest.json}
ISO_TIMESTAMP=${BORGE_EVIDENCE_ISO_TIMESTAMP:-2026-08-25T02:08:01Z}
MAX_BYTES=${BORGE_EVIDENCE_CD_MAX_BYTES:-650000000}
COLLECTION=${BORGE_EVIDENCE_COLLECTION:-stages-0-8}
EVIDENCE_GIT_REF=${BORGE_EVIDENCE_GIT_REF:-refs/tags/v0.8.0}

for tool in git python3 xorriso sha256sum; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "build-evidence-isos: required tool not found: $tool" >&2
        exit 69
    fi
done

PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 python3 \
    "$ROOT/scripts/verify-evidence.py" \
    --manifest "$MANIFEST" \
    --evidence-dir "$EVIDENCE_DIR"

mkdir -p "$ISO_DIR"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/borge-evidence-iso.XXXXXX")
cleanup() {
    chmod -R u+w "$WORK" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT
STAGING=$WORK/staging
READBACK=$WORK/readback
mkdir -p "$STAGING/artifacts" "$STAGING/repository" "$STAGING/documentation" "$READBACK"

mapfile -t ARTIFACTS < <(
    PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 python3 - "$MANIFEST" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    catalog = json.load(source)
for artifact in catalog["artifacts"]:
    print(artifact["filename"])
PY
)

for filename in "${ARTIFACTS[@]}"; do
    cp -p "$EVIDENCE_DIR/$filename" "$STAGING/artifacts/$filename"
done
cp -p "$MANIFEST" "$STAGING/MANIFEST.json"
cp -p "$ROOT/docs/EVIDENCE.md" "$STAGING/documentation/EVIDENCE.md"

# Multi-threaded pack generation can choose different deltas from one run to the next.
# One thread makes the bundle stable, which in turn makes the whole ISO reproducible.
git -C "$ROOT" -c pack.threads=1 bundle create \
    "$STAGING/repository/borge.git.bundle" "$EVIDENCE_GIT_REF"
LC_ALL=C git bundle verify "$STAGING/repository/borge.git.bundle" 2>&1 \
    | sed "s|$STAGING/repository/borge.git.bundle|repository/borge.git.bundle|" \
    >"$STAGING/repository/git-bundle-verify.txt"

EVIDENCE_COMMIT=$(git -C "$ROOT" rev-parse "$EVIDENCE_GIT_REF^{commit}")
MANIFEST_SHA256=$(sha256sum "$MANIFEST" | awk '{print $1}')
XORRISO_VERSION=$(xorriso -version 2>&1 | sed -n '1p')
SOURCE_DATE_EPOCH=$(date -u -d "$ISO_TIMESTAMP" +%s)
export SOURCE_DATE_EPOCH

{
    echo "borge evidence ISO master"
    echo
    echo "Collection: $COLLECTION"
    echo "Evidence Git ref: $EVIDENCE_GIT_REF"
    echo "Evidence Git commit: $EVIDENCE_COMMIT"
    echo "Catalog SHA-256: $MANIFEST_SHA256"
    echo "Image timestamp: $ISO_TIMESTAMP"
    echo "SOURCE_DATE_EPOCH: $SOURCE_DATE_EPOCH"
    echo "Builder: $XORRISO_VERSION"
    echo "Catalog: evidence/manifest.json in the named Git commit"
    echo
    echo "The ZIPs were created earlier and catalogued retrospectively."
    echo "No detached signatures or RFC 3161 tokens are claimed."
    echo "See documentation/EVIDENCE.md for scope, verification, and custody policy."
} >"$STAGING/README.txt"

(
    cd "$STAGING"
    find . -type f ! -name ISO-CONTENTS.sha256 -print0 \
        | sort -z \
        | xargs -0 sha256sum >ISO-CONTENTS.sha256
)

PAYLOAD_BYTES=$(du -sb "$STAGING" | awk '{print $1}')
if (( PAYLOAD_BYTES > MAX_BYTES )); then
    echo "build-evidence-isos: payload is $PAYLOAD_BYTES bytes; CD ceiling is $MAX_BYTES" >&2
    echo "build-evidence-isos: partition the catalog before producing optical masters" >&2
    exit 65
fi

DATE_TAG=$(date -u -d "$ISO_TIMESTAMP" +%Y%m%d)
ISO_DATE=$(date -u -d "$ISO_TIMESTAMP" +%Y%m%d%H%M%S00)
ISO_NAME="borge-evidence-${COLLECTION}-${DATE_TAG}.iso"
ISO_PATH="$ISO_DIR/$ISO_NAME"
if [[ -e "$ISO_PATH" || -e "$ISO_PATH.sha256" ]]; then
    echo "build-evidence-isos: refusing to overwrite existing master $ISO_PATH" >&2
    exit 73
fi

VOLUME_ID=$(printf 'BORGE_EV_%s' "$COLLECTION" | tr '[:lower:].-' '[:upper:]__' | cut -c1-32)
TMP_ISO=$WORK/$ISO_NAME
xorriso -as mkisofs \
    -quiet \
    -r \
    -J \
    -V "$VOLUME_ID" \
    -publisher "borge project" \
    -p "borge evidence archive" \
    --modification-date="$ISO_DATE" \
    --set_all_file_dates "$ISO_DATE" \
    -o "$TMP_ISO" \
    "$STAGING"

xorriso -osirrox on -indev "$TMP_ISO" -extract / "$READBACK" >/dev/null 2>&1
(
    cd "$READBACK"
    sha256sum -c ISO-CONTENTS.sha256
)

mv "$TMP_ISO" "$ISO_PATH"
(
    cd "$ISO_DIR"
    sha256sum "$ISO_NAME" >"$ISO_NAME.sha256"
)
xorriso -indev "$ISO_PATH" -find / -type f -exec lsdl -- \
    >"$ISO_PATH.contents.txt" 2>&1

echo "ISO master: $ISO_PATH"
echo "ISO SHA-256: $(sha256sum "$ISO_PATH" | awk '{print $1}')"
echo "Payload bytes: $PAYLOAD_BYTES"
