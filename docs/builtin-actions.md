# Built-in actions

Built-in actions ship with the runner, so a job needs no action download and no Node in its image.

```yaml
- uses: builtin:checkout
```

## checkout

Works like `actions/checkout` with these inputs. Any other input fails the step.

| Input | Default |
| --- | --- |
| `repository` | `github.repository` |
| `ref` | the triggering commit, or the default branch of another repository |
| `token` | `github.token` |
| `path` | workspace root |
| `fetch-depth` | `1`, `0` for full history |

With `gitea-runner exec`, checking out the workflow's own repository copies your local working directory, unless `--no-skip-checkout` is set.
