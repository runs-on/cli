# CLI guide

Rules for the `roc` CLI module, published from `cli/` in `runs-on/monorepo` to
`runs-on/cli`. Edit it and open pull requests in the monorepo, never in the
mirror; `../` paths and root `make` targets exist only there.

## Rules

- Update `README.md` and the `runs-on/runs-on.com` page
  `src/content/docs/maintenance/troubleshooting/cli.mdx` when commands, flags,
  exit codes, output, or a command's AWS and `gh` access change.
- Never hand-edit version strings in `README.md` or `action.yml`; the root
  `make sync-metadata` rewrites them from `VERSION`, and CI checks.
- Never import root-module packages (`runs-on/pkg/...`): the mirror builds roc
  without `../go.work` (`../docs/GO_MODULES.md`). Copy the code and
  cross-reference both copies, as `internal/cli/cleanup_scope.go` does.
- Read Flex and Fleet job state only through the job diagnostics resolver
  (`../terraform/lambdas/job_diagnostics_resolver.js`). Add new job facts to its
  JSON response; never read the stack's DynamoDB tables from roc.
- Add a new stack metadata key to every writer of the `stack-config` and
  `fleet-config` secrets: `../cloudformation/template.yaml` and `main.tf` in
  `../terraform/modules/control_plane/control_plane_flex/` and
  `control_plane_fleet/`.
- Users upgrade roc and stacks separately: treat new resolver fields and secret
  keys as optional, and skip the step or ask for a stack upgrade when missing.
- Run `validateStackProduct` on the resolver response before a job-URL command
  acts, so roc never acts on a job from another stack or product.
- Keep `roc lint` and `roc version` free of AWS and stack access: the GitHub
  Action, the pre-commit hook, and CI run them without credentials.
- roc ships for Windows: guard Unix-only calls by `runtime.GOOS`, as
  `internal/cli/connect.go` does for `syscall.Exec`.

## Commands

- The `build` job of `CLI / Test` builds roc as the mirror and release do:
  without `../go.work`, for Linux, Windows, and macOS. To check before pushing,
  run `GOWORK=off GOOS=windows mise exec -- go build ./...`, then again with
  `GOOS=darwin`.
