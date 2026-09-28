#!/bin/bash
set -euo pipefail

: "${E2E_CONCURRENCY:?E2E_CONCURRENCY is required}"
: "${E2E_JOB_IMAGE:?E2E_JOB_IMAGE is required}"
: "${E2E_GITEA_IMAGE:?E2E_GITEA_IMAGE is required}"
: "${SERVICE_IMAGE:?SERVICE_IMAGE is required}"

docker pull "$E2E_JOB_IMAGE"
docker pull "$SERVICE_IMAGE"
docker pull "$E2E_GITEA_IMAGE"

exec "${GO:-go}" test -tags e2e -count=1 -parallel "$E2E_CONCURRENCY" -timeout 20m -v ./e2e/...
