# Release process

Releases follow the lab pipeline: merge to `main`, then tag. A release is the
tag; nothing is installed from a developer's machine (ADR 0012).

1. Confirm `main` CI is green. The `msi` job must have passed: it builds the
   MSI from the same zip the release uses and proves a fresh install, a
   major upgrade, the zip's `install.ps1`/`uninstall.ps1`, and clean
   uninstalls on a Windows runner.
2. Tag: `git tag -a vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`. MSI versions
   are numeric, so MAJOR and MINOR must stay at or below 255.
3. The `release` workflow:
   - refuses a tag that is not `vMAJOR.MINOR.PATCH` or not on `main`;
   - on Linux, runs `make check` and `make package VERSION=vX.Y.Z`: builds
     the binaries with the version embedded, fetches the llama.cpp runtime
     pinned in `packaging/llama-cpp.json` (build fails on a SHA-256
     mismatch), and zips the file set with `install.ps1`/`uninstall.ps1`;
   - on Windows, builds `benchwarmer-X.Y.Z-x64.msi` from that zip with WiX
     (`packaging/build-msi.ps1`, version pinned there) and runs
     `packaging/smoke.ps1` for the MSI and the zip;
   - publishes the MSI, `benchwarmer-vX.Y.Z-windows-amd64.zip` and
     `SHA256SUMS` to the GitHub release. Re-running the workflow for the same
     tag replaces the assets instead of failing.
4. Install per [docs/deploy/install.md](deploy/install.md).

Releases are unsigned until a code-signing certificate exists; the Defender
ASR path exclusion is what allows them to run.

## Building locally

```sh
make stage VERSION=v1.2.3     # dist/stage/: the release file set
make package VERSION=v1.2.3   # plus dist/benchwarmer-v1.2.3-windows-amd64.zip
```

Both work on Linux. The MSI needs Windows and the .NET SDK:
`.\packaging\build-msi.ps1 -StageDir dist\stage`.

## Updating llama.cpp

Change `tag`, `asset`, `url` and `sha256` (and `license_url` and
`license_sha256` for the tag's `LICENSE`) in `packaging/llama-cpp.json`. Take
the hashes from files you downloaded and checked, not from the pin you are
replacing. Merge through CI like any other change; the runtime ships in the
next release.
