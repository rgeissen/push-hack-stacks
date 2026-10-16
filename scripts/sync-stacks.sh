#!/usr/bin/env bash
# scripts/sync-stacks.sh [ref] -- re-vendor Stacks from ../schwung-stacks at
# a commit (default: its HEAD), and record the ref. The vendored files are
# never edited here: Push-specific code lives in src/dsp, around them.
#
#   ./scripts/sync-stacks.sh            # HEAD of ../schwung-stacks
#   ./scripts/sync-stacks.sh 0bccb37    # a given commit
#   ./scripts/sync-stacks.sh --check    # exit 1 if vendored != recorded ref
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="${STACKS_REPO:-${ROOT}/../schwung-stacks}"
DST="${ROOT}/src/upstream/stacks"
FILES=(src/dsp/stacks.c src/host/plugin_api_v1.h src/host/midi_fx_api_v1.h
       src/host/move_info.h src/module.json src/web_ui.html src/help.json)

[[ -d "${SRC}/.git" ]] || { echo "no Stacks repo at ${SRC} (set STACKS_REPO)" >&2; exit 1; }

if [[ "${1:-}" == "--check" ]]; then
    ref="$(cat "${DST}/UPSTREAM_REF")"
    bad=0
    for f in "${FILES[@]}"; do
        if ! git -C "${SRC}" show "${ref}:${f}" | cmp -s - "${DST}/${f}"; then
            echo "differs from ${ref}: ${f}"; bad=1
        fi
    done
    [[ ${bad} == 0 ]] && echo "vendored files match ${ref}"
    exit ${bad}
fi

ref="$(git -C "${SRC}" rev-parse "${1:-HEAD}")"
for f in "${FILES[@]}"; do
    mkdir -p "$(dirname "${DST}/${f}")"
    git -C "${SRC}" show "${ref}:${f}" > "${DST}/${f}"
done
echo "${ref}" > "${DST}/UPSTREAM_REF"
echo "vendored Stacks at ${ref}; run make test"
