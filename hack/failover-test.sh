#!/usr/bin/env bash
# FC/CSI resilience tests under continuous direct I/O (deploy/tests/io-load.yaml), run ON the node hosting the pod.
#   hack/failover-test.sh [path|restarts|allpaths|all]
#   path      one path of the test LUN offline 60 s           -> expect 0 errors, no slow writes
#   restarts  restart the driver pods (RESTART_LABELS)          -> expect 0 errors
#   allpaths  all paths offline 180 s                           -> expect ~120 s stall (no_path_retry 24), then
#                                                                  fast errors, recovery right after paths return
# Only SCSI paths whose WWID matches the test PVC's serial are touched.
set -uo pipefail
cd "$(dirname "$0")/.."
NS=sc-fail-test
STORAGE_CLASS="${STORAGE_CLASS:-scseries-fc}"
CSI_NS="${CSI_NS:-sc-csi}"
RESTART_LABELS="${RESTART_LABELS:-app=sc-csi-controller app=sc-csi-node}"  # pod label selectors

setup() {
  sed "s/STORAGE_CLASS/$STORAGE_CLASS/g" deploy/tests/io-load.yaml | kubectl apply -f - >/dev/null
  kubectl -n $NS wait --for=condition=Ready pod/io --timeout=420s >/dev/null || { echo "io pod not ready"; exit 1; }
  local pv; pv=$(kubectl -n $NS get pvc io -o jsonpath='{.spec.volumeName}')
  SERIAL=$(kubectl get volumeattachment -o json | python3 -I -c "import json,sys; d=json.load(sys.stdin); \
print([v['status']['attachmentMetadata']['serialNumber'] for v in d['items'] if v['spec']['source'].get('persistentVolumeName')=='$pv'][0])")
  MAP=$(sudo multipath -ll "3$SERIAL" | awk 'NR==1{print $1}')
  PATHS=$(ls "/sys/block/$(basename "$(readlink -f "/dev/mapper/$MAP")")/slaves" | tr "\n" " ")
  for p in $PATHS; do
    [ "$(cat /sys/block/$p/device/wwid)" = "naa.$SERIAL" ] || { echo "path $p is not the test LUN, abort"; exit 1; }
  done
  echo "test LUN 3$SERIAL = $MAP, paths: $PATHS"
  sleep 15
}

set_state() { local st="$1"; shift; for p in "$@"; do echo "$st" | sudo tee "/sys/block/$p/device/state" >/dev/null; done; }
errors_since() { kubectl -n $NS logs io --since="$1" | grep -c ' ERR ' ; }
slow_since() { kubectl -n $NS logs io --since="$1" | grep -E 'took ([2-9]|[1-9][0-9]+)s' | grep -vc ' ERR ' ; }

t_path() {
  local first; first=$(echo $PATHS | awk '{print $1}')
  echo "== path: $first offline 60 s"
  set_state offline "$first"; sleep 60; set_state running "$first"; sleep 20
  echo "   errors=$(errors_since 2m) slow_writes=$(slow_since 2m)  (expect 0/0)"
  sudo multipath -ll "$MAP" | grep -E "sd[a-z]+ " | sed 's/^/   /'
}

t_restarts() {
  for t in $RESTART_LABELS; do
    echo "== restart $t"
    kubectl -n "$CSI_NS" delete pod -l $t --wait=true >/dev/null
    kubectl -n "$CSI_NS" wait --for=condition=Ready pod -l $t --timeout=240s >/dev/null
    sleep 20
  done
  echo "   errors=$(errors_since 3m) slow_writes=$(slow_since 3m)  (expect 0/0)"
}

t_allpaths() {
  echo "== allpaths: all paths offline 180 s"
  set_state offline $PATHS; sleep 180; set_state running $PATHS; sleep 30
  kubectl -n $NS logs io --since=5m | grep -E 'took [0-9]{2,}s' | head -1 | sed 's/^/   first stalled write: /'
  echo "   errors=$(errors_since 5m) (expect > 0: I/O failed instead of hanging)"
  kubectl -n $NS logs io --since=40s | grep heartbeat | tail -1 | sed 's/^/   after restore: /'
}

setup
case "${1:-all}" in
  path) t_path ;;
  restarts) t_restarts ;;
  allpaths) t_allpaths ;;
  all) t_path; t_restarts; t_allpaths ;;
esac
kubectl delete ns $NS --wait=true --timeout=300s >/dev/null && echo "cleaned up $NS"
