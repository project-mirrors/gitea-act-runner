# Jobs as Kubernetes pods

A `kubernetes` label runs each job as a pod, without a Docker daemon:

```text
ubuntu-latest:kubernetes://docker.gitea.com/runner-images:ubuntu-latest
```

The pod has a `job` container from the label's image or the job's `container.image`, plus one container per service.

## Setup

In a cluster, the runner uses its pod's service account and namespace. [`examples/kubernetes/kubernetes-pods.yaml`](../examples/kubernetes/kubernetes-pods.yaml) deploys a runner with these permissions:

| Resource | Verbs |
| --- | --- |
| `pods` | `create`, `get`, `list`, `delete` |
| `pods/exec` | `create`, `get` |
| `pods/log` | `get` |
| `secrets` | `create`, `delete` |

Outside a cluster, it uses the current context of the first file in `$KUBECONFIG`, or of `~/.kube/config`, which must authenticate with a token, token file or client certificate. The cluster must run Kubernetes 1.30 or later, and job images need `sh`, `tar`, `tail` and `env`.

## Configuration

```yaml
kubernetes:
  kubeconfig: "" # instead of the service account
  namespace: "" # defaults to the service account's, the kubeconfig context's, or default
  pod_template: # merged under each job pod, a container named job merges into the job container, one named $services into each service container
    spec:
      containers:
        - name: job
          resources:
            limits:
              memory: 4Gi
        - name: $services
          resources:
            limits:
              memory: 1Gi
  pod_templates: # merged over pod_template for jobs whose runs-on has the label they are keyed by
    gpu:
      spec:
        nodeSelector:
          nvidia.com/gpu.present: "true"
```

A job with `runs-on: [linux, gpu]` gets the `linux` and then the `gpu` entry of `pod_templates` merged over `pod_template`, while its image comes from the first of its labels the runner has. A template volume named `workspace` or `act` replaces the `emptyDir` the runner mounts at the workspace or at `/var/run/act`, for example to limit its size. Images are pulled by the cluster's `imagePullPolicy` default, a template's, or always with `container.force_pull`. Like every config value, templates expand `${NAME}` from the runner's environment, so a shell command in a template writes `$${NAME}` for the pod's own variable.

Job pods mount no service account token unless the template sets `automountServiceAccountToken: true`. Job pods and their Secrets are labeled `app.kubernetes.io/managed-by: gitea-runner` and `com.gitea.runner.uuid: <runner UUID>`. The idle cleanup removes those the runner did not get to remove, once they are older than `runner.workdir_cleanup_age`.

## Copying files over the pod network

The runner copies files into the job container, such as the actions a step uses, with `tar` over `pods/exec`. That data goes through the API server, which on a managed cluster often sits in other zones than the nodes, so each byte can cross zones twice. With `transfer.mode` set to `auto` or `direct`, the job container instead fetches directories, such as actions and workspaces, and other copies of 64 KiB or more gzipped from the runner pod. Smaller ones, like step scripts, stay on `pods/exec`, as do copies out of the job container and step output.

```yaml
kubernetes:
  transfer:
    mode: direct # api (default), auto or direct
    host: "" # address the runner listens on and job pods reach it at, defaults to its outbound IP, the pod IP in a cluster
    port: 0 # port it listens on, defaults to a free one
    image: "" # image with gitea-runner on its PATH, defaults to the image of a single-container runner pod in kubernetes.namespace
```

Each job pod then gets an init container named `gitea-runner-transfer` from `image`, which copies its `gitea-runner` into the `act` volume for the job container to fetch with over HTTPS. The modes differ in what happens when a job pod cannot reach the runner, for example because of a NetworkPolicy, or when no `image` is set and the runner is not a single-container pod in `kubernetes.namespace`:

| Mode | Copies | When the pod network does not work |
|---|---|---|
| `api` | through the API server | not used |
| `auto` | over the pod network | the job pod gets its copies through the API server, after one failed attempt, and a warning is logged |
| `direct` | over the pod network | the job fails, so a broken setup shows instead of quietly costing API server traffic again |

In both `auto` and `direct`, a job fails when its init container does, for example when `image` cannot be pulled or has no `gitea-runner` on its PATH. A private `image` needs `imagePullSecrets` in `pod_template`, and a versioned tag keeps job pods on the runner's version. An `initContainers` entry named `gitea-runner-transfer` in `pod_template` merges into the init container, for example to set its `resources`.

The copies include secrets, such as `run` scripts with `${{ secrets.* }}` expanded, SSH keys for checkout and the runner's client key, so a runner only serves them to its own job pods:

- The runner listens with a certificate it generates at start. Its SHA-256 pin reaches the job pod over the `pods/exec` stream, which the API server protects with TLS, and the job pod trusts no other certificate, so nobody on the pod network can read or change a copy.
- Every copy has a one-time token, sent on that stream's stdin, so it shows neither in the command line nor in the API server audit log.
- The runner serves a copy only to a connection from an IP of the job pod it is for. Other pods get a 404 and leave the copy for its job pod. This needs the runner to see the job pods' own addresses, so it does not work for a runner outside the cluster or behind a proxy that rewrites source addresses.

To also keep other pods from connecting at all, allow the port only from the runner's own job pods, which carry its UUID in the `com.gitea.runner.uuid` label. The policy denies every other connection to the runner pod, so it also needs to allow `cache.port`, and both ports need fixed values:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: gitea-runner-transfer
spec:
  podSelector:
    matchLabels:
      app: runner # the runner pod
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: <kubernetes.namespace>
          podSelector:
            matchLabels:
              com.gitea.runner.uuid: <runner UUID, from the .runner file>
      ports:
        - port: 9090 # kubernetes.transfer.port
        - port: 9091 # cache.port
```

## Services

Services share the pod's network, so a step reaches one by its id or `localhost`, and two services cannot listen on the same port. An id that is not a valid hostname is sanitized, so `my_db` is reached as `my-db`. Pass secrets to services through `env`, as `command` is visible in the pod spec. Of `options`, only `--health-*` is supported. They become a startup probe with Docker's defaults, which stops a service that fails its retries, so the job fails as on Docker.

## Unsupported

Docker actions, `docker://` steps, and the `options`, `volumes` and `credentials` of the job container fail the job, as do `volumes` and `credentials` of services. Run such workflows on a [Docker in Docker](../examples/kubernetes/README.md) runner. Pull private images with `imagePullSecrets` in `pod_template`.
