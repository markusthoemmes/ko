# Closure-Commit Builds

By default, Go stamps every binary with the commit checked out when it is built
(`vcs.revision` in `go version -m`). In a repository that builds many binaries,
such as a monorepo, every new commit therefore produces a new binary, and a new
image digest, for every image, even when nothing that goes into a given binary
changed.

Setting `KO_CLOSURE_COMMIT=true` makes `ko` build each binary from the most
recent commit that changed any of its inputs instead. The binary is still
stamped by Go with a real, clean commit, but that commit, and with it the image
digest, only changes when the binary's inputs do.

```plaintext
KO_CLOSURE_COMMIT=true ko build ./cmd/app
```

## How it works

For each binary and platform, `ko`:

1. Lists the binary's inputs with `go list -deps`, using the same environment
   and flags as the build:
   - every file in each of the binary's package directories within the
     repository, including packages from modules replaced with a local
     directory,
   - embedded files,
   - the `go.mod` and `go.sum` of the main module, `go.work` and `go.work.sum`
     in workspace mode, and the `go.mod` of every local replacement,
   - the files in the `kodata` directory, with symlinks resolved.
2. Finds the most recent commit that changed any of them, following first
   parents from `HEAD`.
3. Checks out only the inputs at that commit into a temporary repository that
   borrows objects from yours, and builds from there.

Because the checkout only holds the inputs, an input `ko` failed to list makes
the build fail rather than compile an outdated copy. The checkout never writes
to your repository.

## Requirements

- The build must run inside a git repository with history back to the commits
  in question. A shallow clone is rejected. In CI, fetch full history; a
  partial clone without file contents (`git clone --filter=blob:none`) is
  enough.
- For the image digest to be stable as well, leave `SOURCE_DATE_EPOCH` unset or
  set it to a fixed value, since `ko` writes it into the image.

If an input has local changes, including untracked or ignored files in a
package directory, `ko` logs it and builds that binary from the working tree as
usual.
