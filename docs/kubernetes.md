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

A job with `runs-on: [linux, gpu]` gets the `linux` and then the `gpu` entry of `pod_templates` merged over `pod_template`, while its image comes from the first of its labels the runner has. A template volume named `workspace` or `act` replaces the `emptyDir` the runner mounts at the workspace or at `/var/run/act`, for example to limit its size. Images are pulled by the cluster's `imagePullPolicy` default, a template's, or always with `container.force_pull`.

Job pods mount no service account token unless the template sets `automountServiceAccountToken: true`. Job pods and their Secrets are labeled `app.kubernetes.io/managed-by: gitea-runner` and `com.gitea.runner.uuid: <runner UUID>`. The idle cleanup removes those the runner did not get to remove, once they are older than `runner.workdir_cleanup_age`.

## Services

Services share the pod's network, so a step reaches one by its id or `localhost`, and two services cannot listen on the same port. An id is lowercased and has characters other than letters, digits and `-` replaced by `-`, so the service `my_db` is reached as `my-db`. Pass secrets to services through `env`, as `command` is visible in the pod spec. Of `options`, only `--health-*` is supported. They become a startup probe with Docker's defaults, which stops a service that fails its retries, so the job fails as on Docker.

## Unsupported

Docker actions, `docker://` steps, and the `options`, `volumes` and `credentials` of the job container fail the job, as do `volumes` and `credentials` of services. Run such workflows on a [Docker in Docker](../examples/kubernetes/README.md) runner. Pull private images with `imagePullSecrets` in `pod_template`.
