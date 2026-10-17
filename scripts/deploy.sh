#!/usr/bin/env bash
# scripts/deploy.sh [host] -- dev deploy: copy the binary and hack.json to
# the Push and (re)start it the way push-catalog's init.d script does
# (nice 19, -config hack.json), so on-device timing is what users get.
# No service is registered: a catalog install does that.
#
#   ./scripts/deploy.sh            # push.local
#   ./scripts/deploy.sh 192.168.1.20
#   ./scripts/deploy.sh push.local stop
#   SSH_OPTS="-o HostKeyAlias=push.local" ./scripts/deploy.sh 192.168.1.20
#
# When Stacks was installed from Push Hack Catalog, its init.d service
# owns it (run as root, its state file root's, its port the catalog's):
# then only the binary is replaced, through the service, and the
# installed hack.json -- with the assigned port -- is kept.
set -euo pipefail

HOST="${1:-push.local}"
ACTION="${2:-start}"
DIR=/data/push-hack/hacks/stacks
LOG=/data/push-hack/logs/stacks.log
PIDF=/tmp/push-hack-stacks.pid
read -r -a XOPTS <<< "${SSH_OPTS:-}"
SSH=(ssh -n -o BatchMode=yes -o ConnectTimeout=10 ${XOPTS[@]+"${XOPTS[@]}"} "ableton@${HOST}")
ROOTSSH=(ssh -n -o BatchMode=yes -o ConnectTimeout=10 ${XOPTS[@]+"${XOPTS[@]}"} "root@${HOST}")
SERVICE=/etc/init.d/push-hack-stacks
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

stop_remote() {
    # By pid file, never by name: `pkill -f stacks` matches the ssh shell too.
    "${SSH[@]}" "if [ -f $PIDF ]; then kill \$(cat $PIDF) 2>/dev/null || true; rm -f $PIDF; fi; \
        for i in 1 2 3 4 5 6 7 8 9 10; do pidof stacks >/dev/null || break; sleep 0.3; done"
}

[[ "${ACTION}" == "stop" || -x "${ROOT}/stacks" ]] || { echo "no ./stacks binary: run make build" >&2; exit 1; }

if "${ROOTSSH[@]}" "test -x $SERVICE"; then # installed from the catalog
    if [[ "${ACTION}" == "stop" ]]; then
        "${ROOTSSH[@]}" "$SERVICE stop"
        exit 0
    fi
    scp -q -o BatchMode=yes ${XOPTS[@]+"${XOPTS[@]}"} "${ROOT}/stacks" "root@${HOST}:${DIR}/stacks.new"
    # The service's stop does not wait for the exit (Stacks saves its state
    # first, ~1 s), and its start skips a pid still alive: wait here.
    "${ROOTSSH[@]}" "$SERVICE stop; for i in \$(seq 50); do pidof stacks >/dev/null || break; sleep 0.1; done; \
        mv $DIR/stacks.new $DIR/stacks && $SERVICE start"
    echo "replaced the catalog install's binary on ${HOST}: log ${LOG}"
    exit 0
fi

if [[ "${ACTION}" == "stop" ]]; then
    stop_remote
    echo "stopped"
    exit 0
fi

stop_remote
"${SSH[@]}" "mkdir -p $DIR /data/push-hack/logs"
scp -q -o BatchMode=yes ${XOPTS[@]+"${XOPTS[@]}"} "${ROOT}/stacks" "${ROOT}/hack.json" "ableton@${HOST}:${DIR}/"
# `cd ... ; cmd &`, not `cd ... && cmd &`: the latter backgrounds a subshell,
# which holds ssh's stdout open (ssh never returns) and whose pid is not the
# binary's (stop would miss it). nice and nohup both exec, so $! is stacks.
"${SSH[@]}" "cd $DIR; nohup nice -n 19 ./stacks -config $DIR/hack.json >>$LOG 2>&1 </dev/null & echo \$! > $PIDF"
echo "started on ${HOST}: log ${LOG}, panel http://${HOST}:$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["port"])' "${ROOT}/hack.json")/"
