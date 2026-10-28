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
     the binaries with the version embedded, fetches the llama-server runtime
     from the Ollama Windows release pinned in `packaging/ollama-runtime.json`
     (the full zip is verified by SHA-256, build fails on a mismatch; only
     the runtime files are unpacked), and zips the file set with `install.ps1`/`uninstall.ps1`;
   - on Windows, first checks the runtime: `packaging/verify-runtime.ps1` fails
     the release unless every runtime exe and DLL is validly signed by Ollama
     Inc., and `packaging/smoke-runtime.ps1` starts it with every flag
     Benchwarmer passes; then builds `benchwarmer-X.Y.Z-x64.msi` from that zip with WiX
     (`packaging/build-msi.ps1`, version pinned there), Authenticode-signs this
     repo's own `benchwarmer.exe`, `bwtray.exe` and `bwprobe.exe` before the MSI
     is built and then the MSI itself (org shared action
     `sentania-labs/.github/.github/actions/windows-sign`, Azure Artifact
     Signing), repacks the zip from the signed files, and runs
     `packaging/smoke.ps1` for the MSI and the zip;
   - publishes the MSI, `benchwarmer-vX.Y.Z-windows-amd64.zip` and
     `SHA256SUMS` to the GitHub release. Re-running the workflow for the same
     tag replaces the assets instead of failing.
4. Install per [docs/deploy/install.md](deploy/install.md).

Release binaries are Authenticode signed through the lab's Azure Artifact
Signing profile, so the publisher in a file's properties is the
`sentania-labs` certificate: `benchwarmer.exe`, `bwtray.exe` and `bwprobe.exe`
are signed before WiX packs them, and the MSI is signed afterwards. The bundled
runtime is Ollama's own build and keeps Ollama Inc.'s signature. `SHA256SUMS`
covers the signed files. The Defender ASR path exclusion is expected to be
no longer required; it stays documented as a recommended fallback until a fresh
install is verified under rule 01443614 in Block mode with no exclusion.

## Building locally

```sh
make stage VERSION=v1.2.3     # dist/stage/: the release file set
make package VERSION=v1.2.3   # plus dist/benchwarmer-v1.2.3-windows-amd64.zip
```

Both work on Linux. The MSI needs Windows and the .NET SDK:
`.\packaging\build-msi.ps1 -StageDir dist\stage`.

## Updating the runtime

The runtime is the `llama-server` in Ollama's `ollama-windows-amd64.zip`
(about 1.6 GB because it carries CUDA and ROCm; the build keeps about 80 MB of
it: `lib/ollama/*.exe`, `*.dll`, license files and `vulkan/*.dll`, laid out
under `runtime\vulkan\` exactly as in `lib/ollama`). Ollama's llama.cpp trails
upstream by days to weeks, so the update cadence is Ollama's.

Change `ollama_version`, `llama_cpp_tag` (the llama.cpp commit that release
bundles), `url`, `sha256`, and the two license URLs and hashes in
`packaging/ollama-runtime.json`. Take the hashes from files you downloaded and
checked, not from the pin you are replacing. CI caches the full zip keyed by
its hash, so a new pin downloads it once.

Merge through CI like any other change: the `msi` job runs
`packaging/verify-runtime.ps1` (every runtime exe and DLL must be validly
signed by Ollama Inc., so an unsigned or re-signed drop fails the build) and
`packaging/smoke-runtime.ps1` (starts the runtime with every flag Benchwarmer
passes and answers `/health`). The GitHub runner has no GPU, so Vulkan on real
hardware is checked on a real machine.
