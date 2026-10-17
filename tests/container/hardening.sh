#!/usr/bin/env bash
# Fails if compose.yaml or the RUNNING containers regress on the hardening rules in spec §3.
#
# Requires: podman or docker (CONTAINER_RUNTIME, default docker), jq, images built first
# (`set -a; . docker/ffmpeg.env; set +a; $RT compose build`).
#
# Two layers of checks:
#   1. static: the resolved compose model (`compose config --format json`);
#   2. runtime: the real containers. Kernel-enforced state is read from /proc/<pid>/status of each
#      container's init process on the host (CapEff, NoNewPrivs, Seccomp, Uid), so it works for the
#      shell-less distroless API image too. Where a container has a shell we additionally try the
#      forbidden actions (write to rootfs, reach the internet).
# The script ALWAYS uses its own compose project (convertflow-hardening) and host port, starts it itself
# (--no-build) and removes it (down -v) on exit. It never touches the default `convertflow` project, so a
# real deployment and its volumes are safe. `down -v` is refused for any other project name.
# COMPOSE_FILES may list extra "-f file" arguments (used to demonstrate that a weakened config fails).
#   tests/container/hardening.sh --self-test   runs the weakened config and asserts the test FAILS.
set -euo pipefail
SELF=$(readlink -f "${BASH_SOURCE[0]}")   # absolute, resolved before any cd
cd "$(dirname "$SELF")/../.."
RT=${CONTAINER_RUNTIME:-docker}
if [ "${1:-}" = "--self-test" ]; then
  # Run the real test against the weakened override and require that it fails for the RIGHT reasons.
  out=$(COMPOSE_FILES="${SELF_TEST_COMPOSE_FILES--f tests/container/weak.override.yaml}" "$SELF" 2>&1) && rc=0 || rc=$?
  bad=0
  [ "$rc" -ne 0 ] || { echo "SELF-TEST FAIL: weakened config exited 0"; bad=1; }
  grep -q '^ok: ' <<<"$out" || { echo "SELF-TEST FAIL: nested run produced no 'ok:' lines (did not really run)"; bad=1; }
  if grep -qiE 'command not found|No such file or directory' <<<"$out"; then echo "SELF-TEST FAIL: nested run crashed"; bad=1; fi
  for want in \
    "FAIL: every service is read_only" \
    "FAIL: every service drops ALL capabilities and adds none" \
    "FAIL: worker has an empty effective capability set" \
    "FAIL: worker has an empty bounding set" \
    "FAIL: worker rootfs is read-only"; do
    if grep -qF "$want" <<<"$out"; then echo "self-test saw expected: $want"; else echo "SELF-TEST FAIL: expected line missing: $want"; bad=1; fi
  done
  [ "$bad" = 0 ] && echo "self-test ok: weakened config is rejected for the expected reasons"
  exit "$bad"
fi
PROJECT=convertflow-hardening
export CONVERTFLOW_PORT=${CONVERTFLOW_HARDENING_PORT:-18081}
safe_down() {
  case "$PROJECT" in convertflow-hardening*) ;; *) echo "refusing down -v for project $PROJECT" >&2; return 1 ;; esac
  $RT compose -p "$PROJECT" "${CF[@]}" down -v >/dev/null 2>&1 || true
}
# shellcheck disable=SC2206
CF=(-f compose.yaml ${COMPOSE_FILES:-})
fail=0
check() { if ! eval "$2"; then echo "FAIL: $1"; fail=1; else echo "ok:   $1"; fi; }

set -a; . docker/ffmpeg.env; set +a
scripts/gen-secrets.sh >/dev/null
CFG=$($RT compose -p "$PROJECT" "${CF[@]}" config --format json 2>/dev/null)

# ---- static checks on the compose model ------------------------------------------------------
check "every service is read_only" \
  "jq -e '.services | to_entries | all(.value.read_only == true)' <<<\"\$CFG\" >/dev/null"
check "every service drops ALL capabilities and adds none" \
  "jq -e '.services | to_entries | all((.value.cap_drop // []) | index(\"ALL\")) and all((.value.cap_add // []) | length == 0)' <<<\"\$CFG\" >/dev/null"
check "every service sets no-new-privileges" \
  "jq -e '.services | to_entries | all((.value.security_opt // []) | index(\"no-new-privileges:true\"))' <<<\"\$CFG\" >/dev/null"
check "no service is privileged or disables seccomp/apparmor" \
  "! jq -e '.services | to_entries | any(.value.privileged == true or ((.value.security_opt // []) | any(test(\"unconfined\"))))' <<<\"\$CFG\" >/dev/null"
check "no host pid/network/ipc, devices, sysctls or privileged" \
  "! jq -e '.services | to_entries | any(.value.pid == \"host\" or .value.network_mode == \"host\" or .value.ipc == \"host\" or ((.value.devices // []) | length > 0) or ((.value.sysctls // {}) | length > 0) or .value.privileged == true)' <<<\"\$CFG\" >/dev/null"
check "every service has log rotation, core ulimit entry (compose JSON drops the zero values) and a stop grace period on api/worker" \
  "jq -e '(.services | to_entries | all(.value.logging.options.\"max-size\" and (.value.ulimits | has(\"core\")))) and .services.api.stop_grace_period == \"45s\" and .services.worker.stop_grace_period == \"45s\"' <<<\"\$CFG\" >/dev/null"
check "api and worker share identical job timeout/retention/flavor settings" \
  "jq -e '[.services.api.environment, .services.worker.environment] | map({a:.CONVERTFLOW_JOB_TIMEOUT,b:.CONVERTFLOW_RETENTION,c:.CONVERTFLOW_STALE_AFTER,d:.CONVERTFLOW_FLAVOR,e:.CONVERTFLOW_MAX_ATTEMPTS}) | .[0] == .[1]' <<<\"\$CFG\" >/dev/null"
check "every service has pids_limit, mem_limit and cpus" \
  "jq -e '.services | to_entries | all(.value.pids_limit and .value.mem_limit and .value.cpus)' <<<\"\$CFG\" >/dev/null"
check "every service has a healthcheck and restart policy" \
  "jq -e '.services | to_entries | all(.value.healthcheck.test and .value.restart)' <<<\"\$CFG\" >/dev/null"
check "backend network is internal" \
  "jq -e '.networks.backend.internal == true' <<<\"\$CFG\" >/dev/null"
check "worker is only on the backend network" \
  "jq -e '.services.worker.networks | keys == [\"backend\"]' <<<\"\$CFG\" >/dev/null"
check "db is only on the backend network" \
  "jq -e '.services.db.networks | keys == [\"backend\"]' <<<\"\$CFG\" >/dev/null"
check "only the api publishes ports, on loopback by default" \
  "jq -e '([.services | to_entries[] | select(.key != \"api\") | .value.ports // [] | length] | add // 0) == 0 and (.services.api.ports | length == 1 and all(.host_ip == \"127.0.0.1\"))' <<<\"\$CFG\" >/dev/null"
check "secrets are passed as files, not environment" \
  "! jq -e '.services | to_entries | any(.value.environment // {} | to_entries | any(.key | test(\"PASSWORD$|DATABASE_URL$\")))' <<<\"\$CFG\" >/dev/null"
check "external images in compose are pinned by digest" \
  "jq -e '.services | to_entries | all(.value.build != null or (.value.image | test(\"@sha256:[0-9a-f]{64}\$\")))' <<<\"\$CFG\" >/dev/null"
# Every FROM that is not a reference to an earlier build stage must carry a digest.
check "Dockerfile base images are pinned by digest" \
  "! awk 'tolower(\$1)==\"from\"{ref=\$2; if(!(ref in st) && ref !~ /@sha256:[0-9a-f]{64}\$/) print FILENAME\": \"ref; for(i=3;i<=NF;i++) if(tolower(\$i)==\"as\") st[\$(i+1)]=1}' Dockerfile.api Dockerfile.worker | grep -q ."

FLAVOR=${FFMPEG_FLAVOR:-lgpl}; export FFMPEG_FLAVOR=$FLAVOR
for img in convertflow-api:${CONVERTFLOW_VERSION:-dev} convertflow-worker:${CONVERTFLOW_VERSION:-dev}-$FLAVOR; do
  user=$($RT image inspect "$img" --format '{{.Config.User}}' 2>/dev/null || true)
  check "$img image runs as non-root ($user)" "[[ \"$user\" == 65532* ]]"
done

# ---- runtime checks on real containers ---------------------------------------------------------
safe_down
trap safe_down EXIT
FFMPEG_FLAVOR=$FLAVOR $RT compose -p "$PROJECT" "${CF[@]}" up -d --no-build --wait --wait-timeout 120 >/dev/null 2>&1 || true

cid() { $RT ps -q --filter "label=com.docker.compose.project=$PROJECT" --filter "label=com.docker.compose.service=$1" | head -n1; }
status() { grep -E "^$2:" "/proc/$1/status" | awk '{print $2}'; }

for svc in db api worker; do
  id=$(cid "$svc")
  check "$svc container is running" "[ -n \"$id\" ] && [ \"\$($RT inspect $id --format '{{.State.Running}}')\" = true ]"
  [ -n "$id" ] || continue
  pid=$($RT inspect "$id" --format '{{.State.Pid}}')
  check "$svc has an empty effective capability set (CapEff=0)" "[ \"\$(status $pid CapEff)\" = 0000000000000000 ]"
  check "$svc has an empty bounding set (CapBnd=0)" "[ \"\$(status $pid CapBnd)\" = 0000000000000000 ]"
  check "$svc has core dumps disabled (RLIMIT_CORE 0/0)" "grep -E '^Max core file size' /proc/$pid/limits | awk '{print \$5,\$6}' | grep -qx '0 0'"
  check "$svc runs with NoNewPrivs=1" "[ \"\$(status $pid NoNewPrivs)\" = 1 ]"
  check "$svc runs under a seccomp filter (Seccomp=2)" "[ \"\$(status $pid Seccomp)\" = 2 ]"
  # /proc/<pid>/status on the host shows the subuid-mapped id under rootless podman, so ask the runtime
  # for the in-container user names instead (`top` works without a shell in the image).
  check "$svc processes do not run as root" "! $RT top $id user | tail -n +2 | grep -qx root"
  check "$svc rootfs is read-only" "[ \"\$($RT inspect $id --format '{{.HostConfig.ReadonlyRootfs}}')\" = true ]"
  check "$svc rootfs has no writes (diff empty; podman lists only the /etc dir for its own hosts/resolv.conf mounts)" "[ -z \"\$($RT diff $id 2>/dev/null | grep -vE '^C /etc\$')\" ]"
  check "$svc is not privileged" "[ \"\$($RT inspect $id --format '{{.HostConfig.Privileged}}')\" = false ]"
  check "$svc has pids/memory/cpu limits applied" \
    "[ \"\$($RT inspect $id --format '{{.HostConfig.PidsLimit}}')\" -gt 0 ] && [ \"\$($RT inspect $id --format '{{.HostConfig.Memory}}')\" -gt 0 ] && [ \"\$($RT inspect $id --format '{{.HostConfig.NanoCpus}}{{.HostConfig.CpuQuota}}')\" != 00 ]"
  check "$svc healthcheck is healthy" "[ \"\$($RT inspect $id --format '{{.State.Health.Status}}')\" = healthy ]"
  check "$svc environment contains no database password" \
    "! $RT inspect $id --format '{{json .Config.Env}}' | grep -qF \"\$(cat secrets/db_password)\""
  check "$svc has no host mounts other than /run/secrets files" \
    "! $RT inspect $id --format '{{range .Mounts}}{{if eq .Type \"bind\"}}{{.Destination}}{{println}}{{end}}{{end}}' | grep -v '^/run/secrets/' | grep -q ."
done

# Shell-based behavioural checks (db and worker images contain a shell; the distroless API does not).
for svc in db worker; do
  id=$(cid "$svc"); [ -n "$id" ] || continue
  check "$svc: writing to the root filesystem fails" "! $RT exec $id sh -c 'touch /hardening-probe 2>/dev/null'"
done
wid=$(cid worker)
if [ -n "$wid" ]; then
  check "worker: has no outbound connectivity (internal network)" \
    "! $RT exec $wid bash -c 'timeout 4 bash -c \"</dev/tcp/1.1.1.1/53\"' 2>/dev/null"
  check "worker: cannot resolve external names" "! $RT exec $wid bash -c 'timeout 4 getent hosts example.com' >/dev/null 2>&1"
  check "worker: scratch space is tmpfs" "$RT exec $wid sh -c 'grep -E \" /tmp tmpfs \" /proc/mounts' >/dev/null"
fi
aid=$(cid api)
if [ -n "$aid" ]; then
  check "api: published port bound to 127.0.0.1 only" \
    "[ \"\$($RT inspect $aid --format '{{range \$p,\$b := .NetworkSettings.Ports}}{{range \$b}}{{.HostIp}} {{end}}{{end}}')\" = '127.0.0.1 ' ]"
  check "api: image has no shell" "! $RT exec $aid /bin/sh -c true >/dev/null 2>&1"
fi
bn=$($RT network ls --format '{{.Name}}' | grep -E "^${PROJECT}_backend$" | head -n1 || true)
check "runtime backend network is internal" "[ \"\$($RT network inspect $bn --format '{{.Internal}}')\" = true ]"

exit $fail
