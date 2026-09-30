### Run `gitea-runner` in a Docker Container

The recommended Docker-in-Docker image runs its own Docker daemon for jobs, so they never get the host's Docker socket:

```sh
docker run --privileged \
  -e GITEA_INSTANCE_URL=http://192.168.8.18:3000 \
  -e GITEA_RUNNER_REGISTRATION_TOKEN=<runner_token> \
  -v "$PWD/data:/data" \
  -v runner-docker:/var/lib/docker \
  --name my_runner gitea/runner:nightly-dind
```

The volumes keep the runner's registration and the job image cache. See [image flavours](../../README.md#image-flavours) for alternatives.
