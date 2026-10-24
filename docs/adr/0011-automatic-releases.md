# 11. Release automatically on every green push to main

Status: Accepted

## Context

Releases were cut by pushing `vX.Y.Z` tags by hand, so fixes on `main` reached
users of `latest` only when someone remembered to tag. A tag or release
created by CI with `GITHUB_TOKEN` does not start another workflow run, so CI
cannot tag and then rely on the tag build to publish the image.

## Decision

- Every push to `main` that passes all CI jobs is a release. The version is
  `<VERSION>.<run number>`, where the `VERSION` file holds `major.minor`.
- The same CI run tags the image `X.Y.Z`, `X.Y` and `latest` (besides `main`
  and `sha-…`) and then creates the GitHub Release `vX.Y.Z` with generated
  notes. The release job only gets `contents: write`; only the image job gets
  `packages: write`.
- Pushes that only change `docs/**` or Markdown files do not run CI, so they
  do not release. Manually pushed `v*` tags still build as before.

## Consequences

- No manual release step; `latest` always matches the newest green `main`.
- Patch numbers are not contiguous (run numbers also count PR and skipped
  runs), and they say nothing about the size of the change.
- Changing major or minor means editing `VERSION`.
