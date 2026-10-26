#!/bin/bash
set -euo pipefail

: "${E2E_CONCURRENCY:?E2E_CONCURRENCY is required}"
: "${E2E_JOB_IMAGE:?E2E_JOB_IMAGE is required}"
: "${E2E_GITEA_IMAGE:?E2E_GITEA_IMAGE is required}"
: "${E2E_K3S_IMAGE:?E2E_K3S_IMAGE is required}"
: "${SERVICE_IMAGE:?SERVICE_IMAGE is required}"

platform=$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')
pulls=()
for image in "$E2E_JOB_IMAGE" "$SERVICE_IMAGE" "$E2E_GITEA_IMAGE" "$E2E_K3S_IMAGE"; do
  if [[ "$image" == *@sha256:* ]] && [[ "$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image" 2>/dev/null)" == "$platform" ]]; then
    continue
  fi
  docker pull "$image" &
  pulls+=("$!")
done

result=0
for pull in "${pulls[@]}"; do
  wait "$pull" || result=$?
done
((result == 0)) || exit "$result"

exec "${GO:-go}" test -tags e2e -count=1 -parallel "$E2E_CONCURRENCY" -timeout 20m -v ./e2e/...
