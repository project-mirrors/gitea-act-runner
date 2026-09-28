#!/usr/bin/env bash
# Generic docker-in-docker test harness.
#
# Builds a dind image variant from the repo Dockerfile, starts its docker daemon over a
# local TCP port, and runs a Go test command against that daemon via DOCKER_HOST. This
# validates the actual docker version and behaviour shipped in the dind image, so any
# daemon-level regression surfaces here (e.g. the "docker cp" break in gitea/runner#981).
# It is deliberately generic: point it at any package/test to exercise the dind daemon.
#
# Usage: scripts/test-dind.sh [target] [-- go-test-args...]
#   target:        dind (default), dind-rootless, or podman to run PODMAN_TEST_IMAGE's API service instead
#   go-test-args:  passed verbatim to `go test`. Defaults cover image env extraction,
#                  symlink copying and a mounted Docker job using cached images, or the
#                  Docker proxy probe for podman.
#
# Env:
#   DIND_TEST_PORT     host port for the daemon (default 32375)
#   DIND_TEST_IMAGE    skip the build and use this prebuilt image instead
#   DIND_TEST_PRELOAD  space-separated images to copy from the host daemon into the fresh one
set -euo pipefail

target="dind"
case "${1:-}" in
  dind|dind-rootless|podman) target="$1"; shift ;;
esac
[ "${1:-}" = "--" ] && shift
default_tests=false

port="${DIND_TEST_PORT:-32375}"
name="gitea-runner-dind-test-$$"
# The host daemon endpoint, captured before DOCKER_HOST is pointed at the fresh dind daemon.
host_docker="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
test_dir=""

cleanup() {
  docker -H "$host_docker" rm -fv "$name" >/dev/null 2>&1 || true
  if [ -n "$test_dir" ]; then
    rm -rf "$test_dir"
  fi
}
trap cleanup EXIT

if [ "$target" = podman ]; then
  image="${PODMAN_TEST_IMAGE:?}"
  daemon_args=("$image" podman system service --time=0 tcp://0.0.0.0:2375)
  [ $# -gt 0 ] || set -- -count=1 -race -run '^TestDockerProxyWithDaemon$' ./act/container/
else
  image="${DIND_TEST_IMAGE:-gitea-runner-${target}:dind-test}"
  if [ -z "${DIND_TEST_IMAGE:-}" ]; then
    echo "==> Building ${target} image"
    docker build --target "$target" -t "$image" .
  fi
  # Override the image entrypoint (s6) and run only dockerd, exposed over insecure TCP.
  # We are testing the daemon the image ships, not the runner supervision tree.
  daemon_args=(-e DOCKER_TLS_CERTDIR= --entrypoint dockerd-entrypoint.sh "$image" --host=tcp://0.0.0.0:2375)
  if [ $# -eq 0 ]; then
    default_tests=true
    set -- -count=1 -race -run '^TestDocker$' ./act/container/
  fi
fi

# How the test process reaches the daemon depends on where it runs:
#   - plain host: publish 2375 on loopback and connect to 127.0.0.1.
#   - inside a container (CI), the daemon is a sibling container, so its published port is on
#     the host, not our loopback; instead attach it to our own network and reach it by name.
self_container=""
if [ -f /.dockerenv ]; then
  self_container="$(cat /proc/sys/kernel/hostname 2>/dev/null || cat /etc/hostname)"
fi
self_network=""
if [ -n "$self_container" ]; then
  self_network="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{"\n"}}{{end}}' "$self_container" 2>/dev/null | head -1)"
fi

# The two cases differ only in how the daemon is exposed and addressed; everything else
# (privileged, name, daemon_args) is shared, so collect just the differing run args and the
# resulting DOCKER_HOST here.
if [ -n "$self_network" ]; then
  echo "==> Starting ${target} daemon on network ${self_network} (reached as ${name}:2375)"
  run_args=(--network "$self_network")
  daemon_host="tcp://${name}:2375"
else
  echo "==> Starting ${target} daemon on tcp://127.0.0.1:${port}"
  run_args=(-p "127.0.0.1:${port}:2375")
  daemon_host="tcp://127.0.0.1:${port}"
fi
# Create the dind container on the host daemon first, then repoint DOCKER_HOST at it: exporting
# DOCKER_HOST before `docker run` would make this `docker run` target the not-yet-existent dind.
docker -H "$host_docker" run -d --privileged --name "$name" "${run_args[@]}" "${daemon_args[@]}" >/dev/null
export DOCKER_HOST="$daemon_host"

echo "==> Waiting for daemon"
for _ in $(seq 1 60); do
  docker version --format 'server docker {{.Server.Version}}' 2>/dev/null && break
  sleep 1
done
if ! docker version --format 'server docker {{.Server.Version}}'; then
  docker -H "$host_docker" logs "$name" >&2
  exit 1
fi

# Seed the fresh daemon with images the host already has (the CI job pulls them in the
# preceding `make test`), so the daemon-facing tests run without registry access.
echo "==> Seeding daemon with cached host images"
preload="${DIND_TEST_PRELOAD:-alpine:latest}"
job_image="${ACT_TEST_IMAGE:-node:24-bookworm-slim}"
if [ "$default_tests" = true ]; then
  preload="$preload $job_image"
fi
for img in $preload; do
  if docker -H "$host_docker" image inspect "$img" >/dev/null 2>&1; then
    docker -H "$host_docker" save "$img" | docker load >/dev/null 2>&1 && echo "  loaded $img" || true
  fi
done

echo "==> Running tests against ${target} daemon"
go test "$@"

if [ "$default_tests" = true ]; then
  if ! docker image inspect "$job_image" >/dev/null 2>&1; then
    echo "mounted Docker test requires ${job_image}, pull it on the host before running this harness" >&2
    exit 1
  fi
  test_dir="$(mktemp -d)"
  echo "==> Building mounted Docker test for the dind container"
  CGO_ENABLED=0 GOOS=linux GOARCH="$(docker -H "$host_docker" image inspect "$image" --format '{{.Architecture}}')" \
    go test -c -o "$test_dir/runner.test" ./act/runner/
  docker -H "$host_docker" exec "$name" mkdir -p /tmp/gitea-runner-proxy-test/testdata
  tar -C "$test_dir" -cf - runner.test -C "$PWD/act/runner" testdata/docker-proxy | \
    docker -H "$host_docker" exec -i "$name" tar -x -C /tmp/gitea-runner-proxy-test
  socket="unix:///var/run/docker.sock"
  users=(0 1000:2375)
  if [ "$target" = dind-rootless ]; then
    socket="unix:///run/user/1000/docker.sock"
    users=(1000 0)
  fi
  for user in "${users[@]}"; do
    proxy_mode="proxy"
    if [ "$user" = 1000:2375 ]; then
      proxy_mode="direct"
    fi
    echo "==> Running mounted Docker job inside ${target} as UID ${user}, expecting ${proxy_mode} access"
    docker -H "$host_docker" exec --user "$user" -w /tmp/gitea-runner-proxy-test \
      -e DOCKER_HOST="$socket" -e ACT_TEST_DOCKER_PROXY="$proxy_mode" -e ACT_TEST_IMAGE="$job_image" \
      "$name" ./runner.test -test.v -test.run '^TestDockerProxyMountedJob$' -test.timeout 3m
  done
  echo "==> Running mounted Docker job in a container given the ${target} socket, expecting proxy access"
  docker -H "$host_docker" exec -e DOCKER_HOST="$socket" "$name" docker run --rm -v "${socket#unix://}:/var/run/docker.sock" -v /tmp/gitea-runner-proxy-test:/data -w /data \
    -e ACT_TEST_DOCKER_PROXY=proxy -e ACT_TEST_IMAGE="$job_image" "$job_image" ./runner.test -test.v -test.run '^TestDockerProxyMountedJob$' -test.timeout 3m
fi
