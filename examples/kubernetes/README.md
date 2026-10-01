## Kubernetes deployments of `gitea-runner`

Jobs can run on Kubernetes in two ways:

- **Job pods**: each job runs as its own pod, without privileged containers and under the cluster's quotas and policies. Docker actions and `docker://` steps are not supported. Deploy with [`kubernetes-pods.yaml`](kubernetes-pods.yaml) and see [docs/kubernetes.md](../../docs/kubernetes.md).
- **Docker in Docker**: jobs run in a Docker daemon next to the runner. Every workflow feature works, but the daemon is privileged, so a job can break out to the node. Deploy with [`dind-docker.yaml`](dind-docker.yaml), [`statefulset-dind.yaml`](statefulset-dind.yaml) for several replicas, [`rootless-docker.yaml`](rootless-docker.yaml) for a rootless daemon, or the [Helm chart](https://gitea.com/gitea/helm-actions). The sidecars in the first two need Kubernetes 1.29 or later.

All examples read the registration token from the `runner-secret` Secret and the Gitea URL from `GITEA_INSTANCE_URL`, and keep the registration in `/data`. They give a stopping pod three hours to finish its jobs, so keep `runner.shutdown_timeout` below `terminationGracePeriodSeconds`.
