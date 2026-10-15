# Official producer fixture

`actions.lock` and `workflow.yml` were emitted by the official gh-actions-lock
command at d5dca22a06ff8590d50b41612038996800cfb336, using its existing
`trackedSHAStubs` HTTP registry for `actions/checkout@v4.2.2`.

The temporary experiment invoked `runCommandWithHTTP` with
`--no-narrow --no-interactive --hostname github.com` and no existing lockfile.
The official writer, not pinact, produced the YAML. The owner/repository IDs are
synthetic (`1`) from the official test mocks. They support offline structural
tests and **must not** be represented as live-authenticated upstream metadata.
No API calls or credentials are needed to run pinact's fixture test.

A live generation attempt with the source-built official CLI failed with
`authentication token not found for host github.com`. Authentication settings
were left unchanged.
