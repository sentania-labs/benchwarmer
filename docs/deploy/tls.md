# HTTPS for the inference endpoint

The inference proxy serves HTTPS when `listen.inference_tls.enabled` is true.
It is required whenever `listen.inference` is not a loopback address. The
management listener (dashboard and API) stays on loopback over plain HTTP.

## Deployment: a certificate from a Microsoft (AD CS) CA

1. **Request** a certificate from an elevated PowerShell on the target. Use a
   template that issues server-authentication certificates with an
   exportable key (for example a copy of *Web Server*):

   ```powershell
   @"
   [NewRequest]
   Subject = "CN=ss8510.example.lan"
   KeyAlgorithm = ECDSA_P256
   KeyLength = 256
   Exportable = TRUE
   MachineKeySet = TRUE
   [Extensions]
   2.5.29.17 = "{text}"
   _continue_ = "dns=ss8510.example.lan&"
   _continue_ = "dns=ss8510"
   [RequestAttributes]
   CertificateTemplate = BenchwarmerWebServer
   "@ | Set-Content req.inf
   certreq -new req.inf req.csr
   certreq -submit req.csr cert.cer       # or submit through the CA web or MMC
   certreq -accept cert.cer
   ```

   List every name clients will use (DNS names and, if needed, IPs) in the
   subject alternative names; clients validate against those, not the CN.

2. **Export** it with its private key and chain to the data folder:

   ```powershell
   $c = Get-ChildItem Cert:\LocalMachine\My | Where-Object Subject -eq 'CN=ss8510.example.lan'
   $pw = Read-Host -AsSecureString 'PFX password'
   New-Item -ItemType Directory -Force C:\ProgramData\Benchwarmer\tls | Out-Null
   Export-PfxCertificate -Cert $c -FilePath C:\ProgramData\Benchwarmer\tls\inference.pfx -Password $pw -ChainOption BuildChain
   # Store the password in a file only the service and administrators can read.
   [Runtime.InteropServices.Marshal]::PtrToStringBSTR([Runtime.InteropServices.Marshal]::SecureStringToBSTR($pw)) |
     Set-Content -NoNewline C:\ProgramData\Benchwarmer\tls\inference.pfx.pass
   icacls C:\ProgramData\Benchwarmer\tls /inheritance:r /grant:r 'SYSTEM:(OI)(CI)F' 'Administrators:(OI)(CI)F'
   ```

   PEM files work too: set `cert_file` to the chain (server certificate
   first) and `key_file` to the key.

3. **Configure** (dashboard > Configuration > Network, or the API):

   ```json
   "listen": {
     "inference": "0.0.0.0:8480",
     "inference_tls": {
       "enabled": true,
       "cert_file": "tls\\inference.pfx",
       "pfx_password_file": "tls\\inference.pfx.pass"
     }
   }
   ```

   Listener changes need a service restart. Clients must trust the issuing
   CA; domain-joined machines already do when the CA is in the enterprise
   trust store.

4. **Renew** by exporting the renewed certificate over the same file. The
   service notices the change within 30 seconds and switches without a
   restart (event `tls_certificate_reloaded`). If the new file cannot be
   loaded it keeps serving the old certificate and records
   `tls_certificate_problem`. It warns daily from 21 days before expiry.

## Testing: self-signed

Set `enabled: true` and `self_signed: true` with no `cert_file`. The service
generates a one-year ECDSA certificate in `C:\ProgramData\Benchwarmer\tls\`
covering localhost, the loopback addresses, the host name, and any names in
`self_signed_hosts`. Clients must be given `self-signed.crt` to trust it. Do
not use it in deployment.
