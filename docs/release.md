# Release process

Releases follow the lab pipeline: merge to `main`, then tag.

1. Confirm `main` CI is green (Linux and Windows jobs).
2. Tag: `git tag -a vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`.
3. The `release` workflow refuses a tag that is not `vMAJOR.MINOR.PATCH` or
   not on `main`, builds the Windows binaries with the version embedded,
   runs the tests, and publishes `benchwarmer-vX.Y.Z-windows-amd64.zip`
   containing `benchwarmer.exe`, `bwtray.exe`, `bwprobe.exe`, `install.ps1`,
   `uninstall.ps1`, `config.example.json`, and `VERSION`.
4. Install on the target from an elevated PowerShell in the unzipped folder:
   `.\install.ps1` (see ADR 0009). Upgrades use the same command.

Releases are unsigned until a code-signing certificate exists; the Defender
ASR path exclusion is what allows them to run.
