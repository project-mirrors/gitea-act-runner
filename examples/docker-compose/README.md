### Running `gitea-runner` using `docker-compose`

This uses the recommended Docker-in-Docker image, see [image flavours](../../README.md#image-flavours).

```yml
...
  gitea:
    image: gitea/gitea
    ...
    healthcheck:
      # checks availability of Gitea's front-end with curl
      test: ["CMD", "curl", "-f", "<instance_url>"]
      interval: 10s
      retries: 3
      start_period: 30s
      timeout: 10s
    environment:
      # GITEA_RUNNER_REGISTRATION_TOKEN can be used to set a global runner registration token.
      # The Gitea version must be v1.23 or higher.
      # It's also possible to use GITEA_RUNNER_REGISTRATION_TOKEN_FILE to pass the location.
      # - GITEA_RUNNER_REGISTRATION_TOKEN=<user-defined registration token>

  runner:
    image: gitea/runner:latest-dind
    restart: always
    privileged: true
    depends_on:
      gitea:
        # required so runner can attach to gitea, see "healthcheck"
        condition: service_healthy
        restart: true
    volumes:
      - ./data/runner:/data
    environment:
      - GITEA_INSTANCE_URL=<instance url>
      # When using Docker Secrets, it's also possible to use
      # GITEA_RUNNER_REGISTRATION_TOKEN_FILE to pass the location.
      # The env var takes precedence.
      # Needed only for the first start.
      - GITEA_RUNNER_REGISTRATION_TOKEN=<registration token>
```

### Running `gitea-runner` using rootless Docker-in-Docker

On AppArmor hosts such as Ubuntu and Debian, `security_opt` is needed for the daemon to start. Docker's `docker-ce-rootless-extras` package provides the `rootlesskit` profile.

```yml
...
  runner:
    image: gitea/runner:latest-dind-rootless
    restart: always
    privileged: true
    security_opt:
      - apparmor=rootlesskit
    depends_on:
      gitea:
        condition: service_healthy
        restart: true
    volumes:
      - ./data/runner:/data
    environment:
      - GITEA_INSTANCE_URL=<instance url>
      - DOCKER_HOST=unix:///var/run/user/1000/docker.sock
      # Use slirp4netns instead of vpnkit for significantly better network throughput.
      - DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns
      - DOCKERD_ROOTLESS_ROOTLESSKIT_MTU=65520
      # When using Docker Secrets, it's also possible to use
      # GITEA_RUNNER_REGISTRATION_TOKEN_FILE to pass the location.
      # The env var takes precedence.
      # Needed only for the first start.
      - GITEA_RUNNER_REGISTRATION_TOKEN=<registration token>
```
