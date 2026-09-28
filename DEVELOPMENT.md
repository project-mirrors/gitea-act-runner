# Development

## Job log line format

Gitea stores one log row per line and its web UI decodes the payload, so getting the encoding
wrong never fails a test here, it only shows up in the browser.

**A row cannot contain a real newline.** `FormatLog` rewrites `\n` to a literal backslash-n and
truncates at 64 KiB on a byte boundary.

**The payload of a line starting with a recognised prefix is decoded**, with the escape set
depending on the prefix:

| prefix | decodes |
| --- | --- |
| `##[error]` `##[warning]` `##[notice]` `##[debug]` `##[group]` `##[endgroup]` `##[add-matcher]` | `%25` `%0D` `%0A` `%3B` `%5D` |
| `::error::` `::warning::` `::notice::` `::debug::` (with or without ` key=value` properties), `::group::` `::endgroup::` `::add-matcher::` | `%25` `%0D` `%0A` |
| `##[command]` `[command]`, or no recognised prefix | nothing |

### Rules

- **Emitting a command line?** Escape the payload with `runner.EscapeCommandData`. One escaper
  covers both forms: it escapes `%` first, so a literal `%3B` becomes `%253B` that the extra
  `##[…]` rules cannot match, and a raw `;` or `]` is never decoded. It is also what makes
  multi-line work, `\n` becomes `%0A` and the UI turns it back into a line break.
- **Forwarding a command from step output?** Leave the payload alone, it arrived escaped and is
  decoded once. Decoding here double-decodes and destroys multi-line.
- **No prefix?** Do not escape, and split multi-line values into one row each.
- **Interpolating a secret?** Masking runs after escaping, so `AppendSecretMasker` registers the
  encoded forms too.
- Command *properties* also escape `%3A` and `%2C`, which the UI never decodes, so the reporter
  decodes exactly those two when folding a location into an annotation.

## Built-in actions

Each lives in `internal/pkg/action/<name>` and is registered in `builtinActions` in
`act/runner/step_action_builtin.go`.

## End-to-end compatibility tests

`make test-e2e` runs the runner against `E2E_GITEA_IMAGE`. It defaults to the nightly image.
It requires Docker and is excluded from `make test`. CI runs stable and nightly variants in
parallel.

The suite shares one Gitea and regular runner. Cache and ephemeral scenarios use isolated
repository runners. A run path is `<workflow>@<ref>` and a log row is `<timestamp>Z <payload>`.
