# Gitea Runner

Gitea Runner runs the jobs of Gitea Actions workflows. Register it with a Gitea instance to run jobs in Docker containers, Kubernetes pods or directly on the host.

## Installation

### Prerequisites

- Linux, macOS or Windows, or FreeBSD.
- Gitea 1.26.2 or newer.
- `git` 2.34.1 or newer.
- `docker`, or `podman` with `podman.socket` enabled, for `docker` labels and Docker actions. See Docker's [install instructions](https://docs.docker.com/engine/install/).

In the job image, or on the host for host jobs:

- `bash` or `sh`, or `pwsh` or `powershell` on Windows.
- `node` for JavaScript actions and `hashFiles()`.

Kubernetes mode needs [a few more tools](docs/kubernetes.md) in job images.

### Download pre-built binary

Download the binary for your platform from [dl.gitea.com](https://dl.gitea.com/gitea-runner/).

### Build from source

```bash
make build
```

### Build a docker image

```bash
make docker
```

## Quickstart

Enable Actions in the configuration file of your Gitea instance:

```ini
[actions]
ENABLED=true
```

### Register

```bash
./gitea-runner register
```

It asks for:

1. The Gitea instance URL. Use its `ROOT_URL`, not `localhost` or `127.0.0.1`.
2. A runner token, from `/admin/actions/runners` on your instance.
3. A runner name, the hostname when left blank.
4. The runner [labels](#labels), the defaults when left blank.

To register without prompts:

```bash
./gitea-runner register --instance https://gitea.example.com --token <token> --no-interactive
```

### Run

```bash
./gitea-runner daemon
```

## Deployment

### Windows service

Register the runner first, keeping `gitea-runner.exe`, `config.yaml` and the `.runner` file in one directory. Then install and start the service from an elevated Command Prompt:

```bat
sc.exe create gitea-runner binPath= "\"C:\gitea-runner\gitea-runner.exe\" daemon --config \"C:\gitea-runner\config.yaml\"" start= auto
sc.exe start gitea-runner
```

The service logs to `gitea-runner.log` next to the executable. Its account needs write access to that directory and access to the tools jobs use.

`sc.exe stop gitea-runner` lets running jobs finish within `runner.shutdown_timeout`. To allow the same on shutdown, raise the service's preshutdown timeout above it, here to six minutes. This takes effect after the next reboot:

```bat
reg.exe add HKLM\SYSTEM\CurrentControlSet\Services\gitea-runner /v PreshutdownTimeout /t REG_DWORD /d 360000 /f
```

### Run with docker

The recommended Docker-in-Docker image runs its own Docker daemon for jobs. Jobs never get the host's Docker socket, which is equivalent to root access on the host.

```bash
docker run --privileged -e GITEA_INSTANCE_URL=https://your_gitea.com -e GITEA_RUNNER_REGISTRATION_TOKEN=<your_token> \
  -v "$PWD/data:/data" -v runner-docker:/var/lib/docker --name my_runner gitea/runner:latest-dind
```

The volumes keep the registration and the image cache across container recreation. The image reads more variables, such as `GITEA_RUNNER_LABELS` and `CONFIG_FILE`, see [scripts/run.sh](scripts/run.sh) and the [Docker example](examples/docker/README.md).

#### Image flavours

| Tag | Docker daemon for jobs |
| --- | --- |
| `latest-dind` (recommended) | Bundled, needs `--privileged`. |
| `latest-dind-rootless` | Bundled, runs as UID 1000 for stronger isolation. Needs `--privileged` and [rootless Docker](https://docs.docker.com/engine/security/rootless/) support on the host. Its image cache is in `/home/rootless/.local/share/docker`. |
| `latest` | None. Mount the host's `/var/run/docker.sock`, or set `DOCKER_HOST`. |

Versioned and `nightly` tags take the same suffixes, like `nightly-dind`. The images target Docker, not Podman.

### Example deployments

See the [examples](examples) directory for more deployments, among them systemd, launchd on macOS, Kubernetes and VMs.

## Configuration

The runner reads an optional YAML file:

```bash
./gitea-runner config init             # write config.yaml, with no option set
./gitea-runner config generate | less  # read what the options do
./gitea-runner -c config.yaml daemon   # -c also works on register and cache-server
```

`config generate` prints [config.example.yaml](internal/pkg/config/config.example.yaml), which documents every option. Copy the lines you need into your file and uncomment them.

### Editing a config file

`config` edits a file in place and keeps its comments, which suits provisioning scripts:

```bash
./gitea-runner config set runner.capacity 4
./gitea-runner config set runner.envs.MY_VAR value
./gitea-runner config add runner.labels 'ubuntu:docker://node:22'
./gitea-runner config remove runner.labels 'ubuntu:docker://node:22'
./gitea-runner config get runner.labels
```

Keys are dotted YAML paths. Unknown keys and invalid values are refused without touching the file. Without `-c`, it uses `config.yaml` from the working directory, then from the binary's directory.

### Environment variables

Config values can read the runner's environment as `${NAME}`, for example `capacity: ${RUNNER_CAPACITY}`. An unset variable fails the load, and `$${NAME}` stays a literal `${NAME}`.

The override variables of earlier releases, such as `GITEA_RUNNER_CAPACITY`, are gone. Set those options in the file instead.

### Labels

Labels decide which jobs a runner accepts and how it runs them. A job's `runs-on` is matched against the label names, and the first match picks where it runs.

A label is written as `<name>[:<schema>[:<args>]]`, with one of these schemas:

- `docker://<image>` runs the job in a container of `<image>`.
- `kubernetes://<image>` runs it as a Kubernetes pod, see [docs/kubernetes.md](docs/kubernetes.md).
- `host`, the default, runs its steps directly on the runner's machine.

With these labels, `runs-on: ubuntu-latest` runs in a container and `runs-on: macos` on the host:

```text
ubuntu-latest:docker://docker.gitea.com/runner-images:ubuntu-latest,macos:host
```

[gitea/runner-images](https://gitea.com/gitea/runner-images) has suitable images. Jobs matching no label run in `runner.default_image`, or on the host when docker is unavailable.

Host jobs still need a Docker daemon for Docker actions and `docker://` steps. Windows hosts do not support those, as on GitHub. A step's background processes end with the job unless it opts out, see [host mode](docs/host-mode.md).

#### Registration vs config labels

Labels are set at registration, with `--labels` or the prompt. To change them later, use one of these, the leftmost set one wins:

```
daemon --labels / GITEA_RUNNER_LABELS   >   runner.labels in the config file   >   labels in the .runner file
```

Changed labels are saved to the `.runner` file and sent to Gitea on startup. When `runner.labels` is set, `register` uses it and ignores `--labels`.

### Tool cache

Setup actions like `setup-go` install tools into `RUNNER_TOOL_CACHE`. `runner.tool_cache_mode` selects what backs it:

| Mode | Tool cache |
| --- | --- |
| `none` (default) | Provided by the job image. Versions it lacks are downloaded in every job. |
| `shared` | One volume per `runner.capacity` slot, reused by later jobs. It hides the tools later job images ship, so use images without them. |

To ship tools in a job image, install them to `/opt/hostedtoolcache/<tool>/<version>/<arch>` with an empty `<arch>.complete` file next to it, as the `-full` [runner images](https://gitea.com/gitea/runner-images) do:

```dockerfile
RUN GO=$(curl -fsSL 'https://go.dev/dl/?mode=json' | grep -oP '"version": "\Kgo1\.26\.[0-9]*' | head -1); \
  DIR="/opt/hostedtoolcache/go/${GO#go}/x64" && \
  mkdir -p "$(dirname "$DIR")" && \
  curl -fsSL "https://dl.google.com/go/${GO}.linux-amd64.tar.gz" | tar -xz -C /tmp && \
  mv /tmp/go "$DIR" && \
  touch "${DIR}.complete"
```

### Proxy

Set the proxy in the runner's environment, for example with systemd `Environment=` or `docker run -e`:

```sh
http_proxy=http://proxy.example:3128
https_proxy=http://proxy.example:3128
no_proxy=gitea.internal,.example.local
```

The runner uses it and passes it on to jobs. Jobs always reach the cache server, `localhost`, their service containers and a `tcp://` Docker daemon directly. Add Gitea to `no_proxy` if it should be reached directly too.

To change the proxy for jobs, use `runner.envs`, a step's `env:` or `$GITHUB_ENV`. Workflow and job level `env:` have no effect on it.

The Docker daemon pulls images with its own proxy settings. The `dind` images pass the variables above to it, for other daemons see [the Docker documentation](https://docs.docker.com/engine/daemon/proxy/).

A password in the proxy URL is hidden in logs, but every step can read it.

## Jobs

### Service containers

When a service declares a healthcheck, in its image or `options`, the job waits for it to become healthy:

```yaml
services:
  postgres:
    image: postgres:18
    options: >-
      --health-cmd pg_isready
      --health-interval 5s
      --health-retries 10
```

A service that turns unhealthy, or is not healthy within `container.service_ready_timeout`, fails the job. Steps reach a service by its id and port, like `psql -h postgres -p 5432`.

Unlike GitHub, jobs running on the host start no services. Give them a `container:` to use services.

### Docker from a job (`GITEA_DOCKER_WORKSPACE`)

Containers a job starts can bind-mount the workspace by its path, like `docker run -v "$PWD:/src"`. Where the Docker daemon sees another path, for example a remote `DOCKER_HOST`, prefix the bind with `GITEA_DOCKER_WORKSPACE`:

```yaml
volumes:
  - ${GITEA_DOCKER_WORKSPACE:-.}/data:/app/data
```

Set `container.sweep: true` to remove the containers, networks and volumes most Linux container jobs create when they end, `docker compose` services included. Leave it off if jobs deploy services to the runner's Docker daemon.

### Caching (`actions/cache`)

Each runner runs its own cache server. As on GitHub, a job restores caches of its own ref, its pull request's base branch and the default branch.

#### Sharing a cache between runners

Run one `gitea-runner cache-server` and point every runner at it with the same secret, for example from `openssl rand -hex 32`:

```yaml
# cache-server.yaml
cache:
  dir: /data/actcache
  port: 8088
  external_secret: "<secret>" # or external_secret_file: /path/to/secret
```

```bash
gitea-runner -c cache-server.yaml cache-server
```

```yaml
# each runner's config
cache:
  external_server: "http://<cache-server-host>:8088/"
  external_secret: "<secret>"
```

Runners can also share one `cache.dir` on a file system with working file locks, at the cost of slower requests.

#### Storing the cache in an S3-compatible bucket

Runners can share a cache in an S3-compatible bucket, such as AWS S3, MinIO or RustFS, without a cache server:

```yaml
cache:
  s3:
    endpoint: "http://minio:9000"
    region: "us-east-1"
    bucket: "runner-cache"
    prefix: "gitea-runner-cache"
    path_style: true
    access_key_file: "/run/secrets/cache-s3-access-key"
    secret_key_file: "/run/secrets/cache-s3-secret-key"
```

Eviction does not apply to the bucket. Add lifecycle rules that expire old caches and abort incomplete multipart uploads.

#### Eviction

Entries unused for `cache.retention` (default `168h`) are removed. A repository over `cache.repo_size_limit` (default `10GB`) loses its least recently used entries. See [config.example.yaml](internal/pkg/config/config.example.yaml) for more limits.

#### Cache service v2

The runner serves the cache service v2 API used by current `actions/cache` releases. It patches them to use it on Gitea, which also makes the stock `actions/upload-artifact` and `actions/download-artifact` v4 work. Artifact calls then go through the cache server, so jobs must be able to reach it.

### Job hooks (`runner.hooks.job_started`, `runner.hooks.job_completed`)

Hook scripts run where the job's steps run, before the first step and after the last. They match GitHub's `ACTIONS_RUNNER_HOOK_JOB_STARTED` and `ACTIONS_RUNNER_HOOK_JOB_COMPLETED`.

Use them for per-job setup no workflow should carry, like registry logins or masking runner-wide secrets. They block the job, and a non-zero exit fails it. See [docs/job-hooks.md](docs/job-hooks.md).

### Secret masking

Secrets and `::add-mask::` values are hidden in job logs, [local job logs](#local-job-logs-logjobdir), job summaries and container names. As on GitHub, a job output containing one is dropped with a warning.

## Operation

### Idle cleanup

While idle, the runner removes leftovers of earlier jobs every `runner.idle_cleanup_interval` (default `10m`):

- Workspaces older than `runner.workdir_cleanup_age` (default `24h`), when `container.bind_workdir` is on. This assumes no other runner shares `container.workdir_parent`.
- Unused Docker networks and volumes of jobs the runner did not tear down.
- Kubernetes job pods older than `runner.workdir_cleanup_age`, with their Secrets.

Set either option to `0` to turn cleanup off.

### Post-task script (`runner.post_task_script`)

A host script that runs after each task's cleanup, for housekeeping like Docker pruning. The runner takes no new jobs until it exits or hits `runner.post_task_script_timeout` (default `5m`). Its output goes to the runner log.

On Windows, use an `.exe`, `.bat` or `.cmd` file, PowerShell scripts are not supported. See [docs/post-task-script.md](docs/post-task-script.md).

### Local job logs (`log.job.dir`)

Set `log.job.dir` to keep a copy of every task's log, masked as in Gitea, with the job's result on the last line. Copies are kept for `log.job.retention` (default `168h`), so keep it above `runner.timeout`. Only the runner's user can read them.

### OpenTelemetry (`OTEL_*`)

Set `OTEL_EXPORTER_OTLP_ENDPOINT` in the runner's environment to send job traces and runner metrics to an OpenTelemetry collector. See [docs/telemetry.md](docs/telemetry.md).
