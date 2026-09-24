# Installing Benchwarmer

Install Benchwarmer from the MSI (Group Policy or `msiexec`) or by hand from
the zip. Both put the same files in the same place and register the same
service. Pick one route per machine. Then copy a model file and choose it in
the dashboard.

Each release on the [releases page](https://github.com/sentania-labs/benchwarmer/releases)
has:

| File | Use it for |
|---|---|
| `benchwarmer-X.Y.Z-x64.msi` | Group Policy Software Installation, `msiexec`, or double-click |
| `benchwarmer-vX.Y.Z-windows-amd64.zip` | Installing by hand with `install.ps1` |
| `SHA256SUMS` | Checking the downloads (`Get-FileHash <file>`) |

The packages are not code-signed yet.

## Before you install

1. **Defender ASR exclusion.** On a managed PC where ASR rule
   `01443614-cd74-433a-b99e-2ecdc07bfc25` is in Block mode, exclude
   `C:\Program Files\Benchwarmer\` from ASR rules by Group Policy first, or
   the service, tray and runtime are blocked. Details:
   [policy requirements](policy-requirements.md).
2. **GPU driver.** Some AMD driver releases (Adrenalin 26.5.1 and later) can
   hang the GPU when it idles or the display turns off, ending in a blue
   screen. Benchwarmer detects the first hang and backs off, but the fix is a
   driver without the defect. See [ADR 0011](../adr/0011-gpu-driver-resets.md)
   for the affected versions and the rollback that works.
3. **Only if other machines will call the inference endpoint (LAN access):**
   - an inbound firewall rule for TCP 8480 to
     `%ProgramFiles%\Benchwarmer\benchwarmer.exe`, scoped to the calling
     hosts, delivered by Group Policy (managed PCs ignore local rules; the
     installer creates none);
   - a certificate for HTTPS in the computer's certificate store
     (`LocalMachine\My`) with every name clients will use. See
     [HTTPS for the inference endpoint](tls.md).

   Without LAN access, everything stays on `127.0.0.1` and neither is needed.
4. **A model file** in GGUF format. Benchwarmer does not download models.
5. **Installed by hand before?** An MSI cannot take over a service that
   `install.ps1` registered. Run `uninstall.ps1` (without `-Purge`) from the
   old release folder first. Your settings, models and history are kept.

## Install with Group Policy

Software Installation runs at computer startup, as SYSTEM, before anyone
signs in.

1. **Share the MSI.** Copy it to a file share and use the UNC path (for
   example `\\fs01\software\Benchwarmer\benchwarmer-1.4.0-x64.msi`), never a
   mapped drive. The computer account reads it, so grant:
   - share permission: *Authenticated Users*, Read;
   - NTFS permission: *Domain Computers* (or *Authenticated Users*), Read &
     execute.
2. **Assign it.** In a GPO linked to the computers' OU: *Computer
   Configuration > Policies > Software Settings > Software installation >
   New > Package*, pick the UNC path, choose **Assigned**.
3. **Restart the computers.** The package installs at the next startup.
   With fast logon optimization it can take two restarts; enabling *Computer
   Configuration > Administrative Templates > System > Logon > Always wait
   for the network at computer startup and logon* makes it one.

**Upgrading.** Add the new MSI to the same GPO as another package, open its
**Upgrades** tab, add the old package, and choose **Package can upgrade over
the existing package**. The new MSI replaces the old version in place: the
service stops (the runtime drains or stops first), files are replaced, and
the service starts again. Settings and data are untouched. Do not leave the
old package assigned without that upgrade link: Group Policy would try to
reinstall the old version, and the newer one refuses the downgrade.

**Removing.** Remove the package from the GPO with **Immediately uninstall
the software from users and computers**. It uninstalls at the next startup.

## Install with msiexec

From an elevated Command Prompt or PowerShell:

```
msiexec /i benchwarmer-1.4.0-x64.msi /qn /l*v "%TEMP%\benchwarmer-install.log"
```

Exit code 0 is success, 3010 is success with a restart needed, anything
else is a failure explained in the log. Upgrade with the same command and
the newer MSI. If someone is signed in during an upgrade, `bwtray.exe` is
in use: Windows may finish replacing it at the next restart (exit code
3010), and their tray runs the old version until then. The service is
upgraded immediately either way. Uninstall from *Settings > Apps*, or:

```
msiexec /x benchwarmer-1.4.0-x64.msi /qn
```

The MSI takes no properties; there is nothing to configure at install time.

## Install by hand from the zip

1. Unblock the download (files from the internet are otherwise refused by
   PowerShell), then unzip:
   ```powershell
   Unblock-File .\benchwarmer-v1.4.0-windows-amd64.zip
   Expand-Archive .\benchwarmer-v1.4.0-windows-amd64.zip -DestinationPath .
   ```
2. From an **elevated** PowerShell in the unzipped folder:
   ```powershell
   Set-ExecutionPolicy -Scope Process Bypass
   .\install.ps1
   ```
   It stops any running copy, copies the files to
   `C:\Program Files\Benchwarmer\`, registers the service, registers the
   tray, starts the service and checks it answers. It warns if the ASR
   exclusion is missing.

To upgrade, run the newer release's `install.ps1` the same way. To remove,
run `.\uninstall.ps1`.

## What gets installed

| Where | What |
|---|---|
| `C:\Program Files\Benchwarmer\` | `benchwarmer.exe` (service), `bwtray.exe` (tray), `bwprobe.exe` (diagnostics), `VERSION`, `config.example.json` |
| `...\runtime\vulkan\` | The bundled llama.cpp runtime (Vulkan build) |
| `...\licenses\` | Third-party licenses |
| Service `Benchwarmer` | Runs as LocalSystem, starts automatically |
| `HKLM\...\CurrentVersion\Run` value `Benchwarmer Tray` | Starts the tray in every user session |
| `C:\ProgramData\Benchwarmer\` | Created by the service itself when it starts: `config.json`, `models\`, `logs\`, `secrets\` (API tokens), `tls\`, the history database |

The service checks and repairs its data folder and permissions every time it
starts: only SYSTEM and Administrators can read it, except `logs\` and
`models\` (readable by local users) and the tray's own token. A deleted
token or a broken permission is fixed by restarting the service.

Neither the MSI nor `install.ps1` creates firewall rules or changes Defender
settings.

## First run

1. **Copy a model** to `C:\ProgramData\Benchwarmer\models\` (needs
   administrator rights).
2. **Open the dashboard.** The tray starts at the next sign-in; its menu
   opens the dashboard. You can also browse to <http://127.0.0.1:8481/> on
   the PC itself. Until a model is chosen, the status shows
   **Setup required**. That is expected, not a fault.
3. **Sign in to change settings.** Choose **Sign in to change settings** in
   the tray menu. Windows asks for administrator approval, then the
   dashboard opens already signed in for that browser tab. Standard users
   can view status but cannot change settings.
4. **Configure.** On the **Configuration** page, pick the model from the
   list of files in `models\`. The runtime and, for LAN access, the HTTPS
   certificate are picked from lists the same way, so no paths need typing.
   Once saved, Setup required clears on its own and the service loads the
   model when the GPU is free.

## Pre-configuring many PCs

A `config.json` placed in `C:\ProgramData\Benchwarmer\` **before the service
first starts** is used as-is instead of the defaults. Start from
`config.example.json` in the install folder, change what you need, and
deliver it with a Group Policy Preferences file item (*Computer
Configuration > Preferences > Windows Settings > Files*). Model files can be
delivered to `models\` the same way.

- Deploy the file preference before assigning the MSI, so the file is in
  place at first start. If the service started first, it has already written
  its defaults.
- Action **Create** copies the file only if none exists, so later changes
  made in the dashboard stick. Action **Replace** makes the GPO the source
  of truth: dashboard changes are overwritten at the next policy refresh
  and the GPO's file takes effect when the service restarts.
- An invalid file does not stop the service: it reports the problem in its
  log and runs on the last settings that worked, or the defaults, without
  overwriting your file.

## Uninstalling and what it keeps

Uninstalling (GPO removal, `msiexec /x`, *Settings > Apps*, or
`uninstall.ps1`) removes the service, the tray's Run value and
`C:\Program Files\Benchwarmer\`. It **keeps** `C:\ProgramData\Benchwarmer\`:
settings, models, history, logs, tokens and certificates, so a reinstall
picks up where it left off.

To remove that too, after uninstalling, from an elevated PowerShell:

```powershell
Remove-Item -Recurse -Force C:\ProgramData\Benchwarmer
```

(`uninstall.ps1 -Purge` does the same for a hand install.)

## Checking an install

```powershell
Get-Service Benchwarmer
Invoke-RestMethod http://127.0.0.1:8481/api/v1/health
Get-Content 'C:\Program Files\Benchwarmer\VERSION'
```

Logs are in `C:\ProgramData\Benchwarmer\logs\`. See
[troubleshooting](../troubleshooting.md).
