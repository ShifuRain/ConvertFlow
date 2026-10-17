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
# If the stack is not running the script starts it (--no-build) and tears it down (down -v) on exit.
# COMPOSE_FILES may list extra "-f file" arguments (used to demonstrate that a weakened config fails).
set -euo pipefail
cd "$(dirname "$0")/../.."
RT=${CONTAINER_RUNTIME:-docker}
# shellcheck disable=SC2206
CF=(-f compose.yaml ${COMPOSE_FILES:-})
fail=0
check() { if ! eval "$2"; then echo "FAIL: $1"; fail=1; else echo "ok:   $1"; fi; }

set -a; . docker/ffmpeg.env; set +a
scripts/gen-secrets.sh >/dev/null
CFG=$($RT compose "${CF[@]}" config --format json 2>/dev/null)

# ---- static checks on the compose model ------------------------------------------------------
check "every service is read_only" \
  "jq -e '.services | to_entries | all(.value.read_only == true)' <<<\"\$CFG\" >/dev/null"
check "every service drops ALL capabilities and adds none" \
  "jq -e '.services | to_entries | all((.value.cap_drop // []) | index(\"ALL\")) and all((.value.cap_add // []) | length == 0)' <<<\"\$CFG\" >/dev/null"
check "every service sets no-new-privileges" \
  "jq -e '.services | to_entries | all((.value.security_opt // []) | index(\"no-new-privileges:true\"))' <<<\"\$CFG\" >/dev/null"
check "no service is privileged or disables seccomp/apparmor" \
  "! jq -e '.services | to_entries | any(.value.privileged == true or ((.value.security_opt // []) | any(test(\"unconfined\"))))' <<<\"\$CFG\" >/dev/null"
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

for img in convertflow-api convertflow-worker; do
  user=$($RT image inspect "$img:${CONVERTFLOW_VERSION:-dev}" --format '{{.Config.User}}' 2>/dev/null || true)
  check "$img image runs as non-root ($user)" "[[ \"$user\" == 65532* ]]"
done

# ---- runtime checks on real containers ---------------------------------------------------------
started=0
if [ -z "$($RT ps -q --filter label=com.docker.compose.project=convertflow)" ]; then
  FFMPEG_FLAVOR=${FFMPEG_FLAVOR:-lgpl} $RT compose "${CF[@]}" up -d --no-build --wait --wait-timeout 120 >/dev/null 2>&1 || true
  started=1
fi
cleanup() { if [ "$started" = 1 ]; then $RT compose "${CF[@]}" down -v >/dev/null 2>&1 || true; fi; }
trap cleanup EXIT

cid() { $RT ps -q --filter label=com.docker.compose.project=convertflow --filter "label=com.docker.compose.service=$1" | head -n1; }
status() { grep -E "^$2:" "/proc/$1/status" | awk '{print $2}'; }

for svc in db api worker; do
  id=$(cid "$svc")
  check "$svc container is running" "[ -n \"$id\" ] && [ \"\$($RT inspect $id --format '{{.State.Running}}')\" = true ]"
  [ -n "$id" ] || continue
  pid=$($RT inspect "$id" --format '{{.State.Pid}}')
  check "$svc has an empty effective capability set (CapEff=0)" "[ \"\$(status $pid CapEff)\" = 0000000000000000 ]"
  check "$svc has an empty bounding set (CapBnd=0)" "[ \"\$(status $pid CapBnd)\" = 0000000000000000 ]"
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
bn=$($RT network ls --format '{{.Name}}' | grep -E '(^|_)backend$' | head -n1 || true)
check "runtime backend network is internal" "[ \"\$($RT network inspect $bn --format '{{.Internal}}')\" = true ]"

exit $fail
