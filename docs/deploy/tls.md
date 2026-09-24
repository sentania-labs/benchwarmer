# HTTPS for the inference endpoint

The inference proxy serves HTTPS when `listen.inference_tls.enabled` is true.
It is required whenever `listen.inference` is not a loopback address. The
management listener (dashboard and API) stays on loopback over plain HTTP.

The certificate comes from the computer's certificate store
(`LocalMachine\My`) or from PEM files. The store is the deployment path: it
is where AD CS enrollment puts a certificate, the private key can stay
non-exportable, and autoenrollment renewals are picked up with no change.
PFX files are not read; import one into the store instead (below).

## Deployment: a certificate from a Microsoft (AD CS) CA

### 1. Get the certificate into LocalMachine\My

Benchwarmer signs through CNG. Prefer a Key Storage Provider (for example
*Microsoft Software Key Storage Provider*, or the *Microsoft Platform Crypto
Provider* for TPM keys); each route below produces one. A key in a legacy
software CryptoAPI provider (CSP) usually works too, because CNG can open
it. A key that CNG cannot open (for example in some hardware CSPs) is
refused with an error naming the legacy provider.

Any one of these:

- **Autoenrollment (fleet).** Publish a server-authentication template (for
  example a copy of *Web Server*, subject built from the DNS name, enroll and
  autoenroll granted to the computers' group). On the template's
  Cryptography tab choose **Key Storage Provider** (Windows Server 2008 or
  later compatibility). Enable certificate autoenrollment in a computer GPO. Each PC enrolls and renews its own
  certificate.
- **Request on the PC.** From an elevated PowerShell. The template must allow
  server authentication, "Supply in the request" for the subject so the SANs
  are honoured, and a Key Storage Provider:

  ```powershell
  @"
  [NewRequest]
  Subject = "CN=ss8510.example.lan"
  KeyAlgorithm = RSA
  KeyLength = 2048
  Exportable = FALSE
  MachineKeySet = TRUE
  ProviderName = "Microsoft Software Key Storage Provider"
  [Extensions]
  2.5.29.17 = "{text}"
  _continue_ = "dns=ss8510.example.lan&"
  _continue_ = "dns=ss8510"
  [RequestAttributes]
  CertificateTemplate = BenchwarmerWebServer
  "@ | Set-Content -Encoding ascii req.inf
  certreq -new -machine req.inf req.csr
  certreq -submit req.csr cert.cer       # or submit through the CA web or MMC
  certreq -accept -machine cert.cer
  ```

- **Import a PFX** (for example a wildcard certificate issued elsewhere).
  `certutil` can force the key into a Key Storage Provider, whatever
  provider the PFX names, and prompts for the password:

  ```powershell
  certutil -csp "Microsoft Software Key Storage Provider" -importpfx .\server.pfx NoExport
  ```

  This imports into `LocalMachine\My` with a non-exportable key.
  `Import-PfxCertificate -CertStoreLocation Cert:\LocalMachine\My` also
  works when the PFX came from a CNG key (a PFX exported from a modern
  Windows machine usually did). If Benchwarmer reports a legacy provider,
  re-import with `certutil` as above. Delete the PFX afterwards.

List every name clients will use (DNS names and, if needed, IPs) in the
subject alternative names; clients validate against those, not the CN.

### 2. Configure

Dashboard > Configuration > Network > Inference HTTPS, or the API:

```json
"listen": {
  "inference": "0.0.0.0:8480",
  "inference_tls": {
    "enabled": true,
    "store_subject": "ss8510.example.lan"
  }
}
```

- `store_subject` matches a DNS name the certificate is valid for
  (wildcards included) or, failing that, text in the subject. The newest
  currently valid match that has a private key and allows server
  authentication wins, so a renewal is picked up by itself.
- `store_thumbprint` pins one certificate instead. A renewal then needs the
  new thumbprint.

Listener changes need a service restart. Clients must trust the issuing CA;
domain-joined machines already do when the CA is in the enterprise trust
store.

### 3. Renewal and failures

- **Store:** re-checked every 10 minutes; a newer matching certificate is
  used from then on (event `tls_certificate_reloaded`).
- **PEM files:** re-checked within 30 seconds of a change.
- **Bad certificate at start:** if the certificate is unusable when the
  service starts, the dashboard and API still run and only the inference
  listener stays down (event `tls_certificate_problem`), so it can be fixed
  from the dashboard.
- **Expiry:** a warning once a day from 21 days before expiry.

## PEM files

Set `cert_file` to the chain (server certificate first) and `key_file` to the
key. Relative paths are inside `C:\ProgramData\Benchwarmer`, which the
service limits to SYSTEM and Administrators. A bad renewal keeps the previous
certificate and records `tls_certificate_problem` once.

## Testing: self-signed

Set `enabled: true` and `self_signed: true` with no store selection or file.
The service generates a one-year ECDSA certificate in
`C:\ProgramData\Benchwarmer\tls\` covering localhost, the loopback addresses,
the host name, and any names in `self_signed_hosts`. Clients must be given
`self-signed.crt` to trust it. Do not use it in deployment.

## Upgrading from a version that read PFX files

A config that still points `cert_file` at a `.pfx` loads, but the inference
listener stays down with a `tls_certificate_problem` event saying to import
the PFX; the dashboard and every other setting keep working. Saving a config
with a `.pfx` `cert_file` is rejected. A leftover `pfx_password_file` is
dropped. Import the PFX (step 1) and switch the config to
`store_subject` or `store_thumbprint`.
