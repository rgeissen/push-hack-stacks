#!/usr/bin/env bash
# scripts/deploy.sh [host] -- dev deploy: copy the binary and hack.json to
# the Push and (re)start it the way push-catalog's init.d script does
# (nice 19, -config hack.json), so on-device timing is what users get.
# No service is registered: a catalog install does that.
#
#   ./scripts/deploy.sh            # push.local
#   ./scripts/deploy.sh 192.168.0.134
#   ./scripts/deploy.sh push.local stop
set -euo pipefail

HOST="${1:-push.local}"
ACTION="${2:-start}"
DIR=/data/push-hack/hacks/stacks
LOG=/data/push-hack/logs/stacks.log
PIDF=/tmp/push-hack-stacks.pid
SSH=(ssh -n -o BatchMode=yes -o ConnectTimeout=10 "ableton@${HOST}")
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

stop_remote() {
    # By pid file, never by name: `pkill -f stacks` matches the ssh shell too.
    "${SSH[@]}" "if [ -f $PIDF ]; then kill \$(cat $PIDF) 2>/dev/null || true; rm -f $PIDF; fi; \
        for i in 1 2 3 4 5 6 7 8 9 10; do pidof stacks >/dev/null || break; sleep 0.3; done"
}

if [[ "${ACTION}" == "stop" ]]; then
    stop_remote
    echo "stopped"
    exit 0
fi

[[ -x "${ROOT}/stacks" ]] || { echo "no ./stacks binary: run make build" >&2; exit 1; }
stop_remote
"${SSH[@]}" "mkdir -p $DIR /data/push-hack/logs"
scp -q -o BatchMode=yes "${ROOT}/stacks" "${ROOT}/hack.json" "ableton@${HOST}:${DIR}/"
# `cd ... ; cmd &`, not `cd ... && cmd &`: the latter backgrounds a subshell,
# which holds ssh's stdout open (ssh never returns) and whose pid is not the
# binary's (stop would miss it). nice and nohup both exec, so $! is stacks.
"${SSH[@]}" "cd $DIR; nohup nice -n 19 ./stacks -config $DIR/hack.json >>$LOG 2>&1 </dev/null & echo \$! > $PIDF"
echo "started on ${HOST}: log ${LOG}, panel http://${HOST}:$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["port"])' "${ROOT}/hack.json")/"
