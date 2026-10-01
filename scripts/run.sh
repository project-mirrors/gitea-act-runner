#!/usr/bin/env bash

if [[ ! -d /data ]]; then
  mkdir -p /data
fi

cd /data

CONFIG_ARG=""
if [[ ! -z "${CONFIG_FILE}" ]]; then
  CONFIG_ARG="--config ${CONFIG_FILE}"
fi
RUNNER_STATE_FILE=${RUNNER_STATE_FILE:-$(gitea-runner ${CONFIG_ARG} config runner-file)}
LABEL_ARGS=""
if [[ ! -z "${GITEA_RUNNER_LABELS}" ]]; then
  LABEL_ARGS="--labels ${GITEA_RUNNER_LABELS}"
fi
EXTRA_ARGS="${LABEL_ARGS}"
if [[ ! -z "${GITEA_RUNNER_EPHEMERAL}" ]]; then
  EXTRA_ARGS="${EXTRA_ARGS} --ephemeral"
fi
# Also pass the labels to the daemon, so that an already registered runner
# picks up changes to GITEA_RUNNER_LABELS instead of keeping the labels it
# was first registered with.
RUN_ARGS="${LABEL_ARGS}"
if [[ ! -z "${GITEA_RUNNER_ONCE}" ]]; then
  RUN_ARGS="${RUN_ARGS} --once"
fi

# In case no token is set, it's possible to read the token from a file, i.e. a Docker Secret
if [[ -z "${GITEA_RUNNER_REGISTRATION_TOKEN}" ]] && [[ -f "${GITEA_RUNNER_REGISTRATION_TOKEN_FILE}" ]]; then
  GITEA_RUNNER_REGISTRATION_TOKEN=$(cat "${GITEA_RUNNER_REGISTRATION_TOKEN_FILE}")
fi

# Use the same ENV variable names as https://github.com/vegardit/docker-gitea-act-runner
test -f "$RUNNER_STATE_FILE" || echo "$RUNNER_STATE_FILE is missing or not a regular file"

if [[ ! -s "$RUNNER_STATE_FILE" ]]; then
  for ((try = 1; ; try++)); do # waits for Gitea like the daemon does, a container may have no restart policy
    gitea-runner register \
      --instance "${GITEA_INSTANCE_URL}" \
      --token    "${GITEA_RUNNER_REGISTRATION_TOKEN}" \
      --name     "${GITEA_RUNNER_NAME:-`hostname`}" \
      ${CONFIG_ARG} ${EXTRA_ARGS} --no-interactive 2>&1 | tee /tmp/reg.log

    if grep -q 'Runner registered successfully' /tmp/reg.log; then
      echo "SUCCESS"
      break
    fi
    if [[ ${GITEA_MAX_REG_ATTEMPTS:-0} -gt 0 ]] && [[ $try -ge $GITEA_MAX_REG_ATTEMPTS ]]; then
      echo "Registration failed after ${try} attempts"
      exit 1
    fi
    echo "Waiting to retry ..."
    sleep 5
  done
fi
# Prevent reading the token from the gitea-runner process
unset GITEA_RUNNER_REGISTRATION_TOKEN
unset GITEA_RUNNER_REGISTRATION_TOKEN_FILE

exec gitea-runner daemon ${CONFIG_ARG} ${RUN_ARGS}
