### BUILDER STAGE
#
#
FROM golang:1.24-alpine AS builder

# Do not remove `git` here, it is required for getting runner version when executing `make build`
RUN apk add --no-cache make git

ARG GOPROXY
ENV GOPROXY=${GOPROXY:-}

COPY . /opt/src/act_runner
WORKDIR /opt/src/act_runner

RUN make clean && make build

### DIND VARIANT
#
#
FROM docker:dind AS dind

RUN apk add --no-cache s6 bash git tzdata

COPY --from=builder /opt/src/act_runner/act_runner /usr/local/bin/act_runner
COPY scripts/run.sh /usr/local/bin/run.sh
COPY scripts/s6 /etc/s6

VOLUME /data

ENTRYPOINT ["s6-svscan","/etc/s6"]

### DIND-ROOTLESS VARIANT
#
#
FROM docker:dind-rootless AS dind-rootless

USER root
RUN apk add --no-cache s6 bash git tzdata

COPY --from=builder /opt/src/act_runner/act_runner /usr/local/bin/act_runner
COPY scripts/run.sh /usr/local/bin/run.sh
COPY scripts/s6 /etc/s6

VOLUME /data

RUN mkdir -p /data && chown -R rootless:rootless /etc/s6 /data

ENV DOCKER_HOST=unix:///run/user/1000/docker.sock

USER rootless
ENTRYPOINT ["s6-svscan","/etc/s6"]

### BASIC VARIANT
#
#
FROM alpine AS basic
RUN apk add --no-cache tini bash git tzdata

COPY --from=builder /opt/src/act_runner/act_runner /usr/local/bin/act_runner
COPY scripts/run.sh /usr/local/bin/run.sh

VOLUME /data

ENTRYPOINT ["/sbin/tini","--","run.sh"]
