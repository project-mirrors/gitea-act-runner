# Built-in actions

Built-in actions ship with the runner, so a job needs no action download and no Node in its image.

```yaml
- uses: builtin:checkout
```

## checkout

Works like `actions/checkout` with these inputs. Any other input fails the step. It runs `git` where the job runs, see the [prerequisites](../README.md#prerequisites).

| Input | Default |
| --- | --- |
| `repository` | `github.repository` |
| `ref` | the triggering ref and commit, or the default branch of another repository |
| `token` | `github.token` |
| `ssh-key` | empty, a private key fetches over SSH instead |
| `ssh-known-hosts` | empty, host keys trusted besides `~/.ssh/known_hosts` |
| `ssh-strict` | `true`, rejects unknown host keys |
| `ssh-user` | the user of the repository's SSH URL, else `git` |
| `persist-credentials` | `true`, later steps of the job can fetch and push with `token` or `ssh-key` |
| `path` | workspace root |
| `clean` | `true`, removes untracked files, also in submodules |
| `filter` | empty, a partial clone filter such as `blob:none` |
| `sparse-checkout` | empty, the paths to check out, one per line |
| `sparse-checkout-cone-mode` | `true`, `false` reads `sparse-checkout` as patterns |
| `fetch-depth` | `1`, `0` for the full history of every branch and tag |
| `fetch-tags` | `false`, `true` fetches every tag |
| `show-progress` | `true`, shows the fetch progress |
| `lfs` | `false`, `true` downloads Git LFS files |
| `submodules` | `false`, `true` checks out submodules, `recursive` also theirs |
| `set-safe-directory` | `true`, lets checkout's `git` work in a directory another user owns |

A branch is checked out as a local branch tracking `origin`, a tag or commit as a detached `HEAD`. A triggering tag that moved since the event fails the step. The step outputs `ref` and `commit` like `actions/checkout`.

Persisted credentials end with the job. `token` is only sent to `github.server_url`, while `ssh-key` is offered to any SSH host, as with `actions/checkout`. The runner's `client_cert_file` is only offered to `github.server_url` and only during the step, so later steps do not get it. Without `ssh-key`, submodule URLs of the form `git@<server host>:` are fetched from `github.server_url` with `token`. With `ssh-key`, the repository is fetched from the SSH URL in the event's `repository.ssh_url`, or from the server's host on port 22.

With `gitea-runner exec`, checking out the workflow's own repository copies your local working directory, unless `--no-skip-checkout` or `ssh-key` is set.
