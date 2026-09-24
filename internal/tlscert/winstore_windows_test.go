//go:build windows

package tlscert

import (
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// newStoreCert creates a non-exportable certificate in LocalMachine\My and
// returns its thumbprint. Needs an elevated process (true on CI runners).
func newStoreCert(t *testing.T, dns, keyArgs string) string {
	t.Helper()
	ps := `$ErrorActionPreference = 'Stop'; Import-Module Microsoft.PowerShell.Security, PKI; $c = New-SelfSignedCertificate -DnsName '` + dns + `' -CertStoreLocation Cert:\LocalMachine\My -KeyExportPolicy NonExportable -NotAfter (Get-Date).AddDays(2) ` + keyArgs + `; $c.Thumbprint`
	cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
	// A PowerShell 7 module path inherited from the runner hides the
	// Windows PowerShell PKI module and the Cert: drive.
	cmd.Env = append(os.Environ(), "PSModulePath=")
	out, err := cmd.CombinedOutput()
	tp := strings.TrimSpace(string(out))
	if err != nil || len(tp) != 40 {
		t.Fatalf("could not create a LocalMachine certificate: %v %s", err, out)
	}
	t.Cleanup(func() {
		c := exec.Command("powershell", "-NoProfile", "-Command", `Import-Module Microsoft.PowerShell.Security; Remove-Item Cert:\LocalMachine\My\`+tp+` -DeleteKey`)
		c.Env = append(os.Environ(), "PSModulePath=")
		_ = c.Run()
	})
	return tp
}

func handshake(t *testing.T, m *Manager, maxVersion uint16) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", m.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })}
	go func() { _ = hs.Serve(ln) }()
	defer hs.Close()
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MaxVersion: maxVersion}}}
	resp, err := c.Get("https://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("handshake (max TLS 0x%x): %v", maxVersion, err)
	}
	defer resp.Body.Close()
	sum := sha1.Sum(resp.TLS.PeerCertificates[0].Raw)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func TestStoreCertificateRSAAndECDSA(t *testing.T) {
	for _, tc := range []struct{ name, keyArgs string }{
		{"rsa", "-KeyAlgorithm RSA -KeyLength 2048 -Provider 'Microsoft Software Key Storage Provider'"},
		{"ecdsa", "-KeyAlgorithm ECDSA_nistP256 -Provider 'Microsoft Software Key Storage Provider'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dns := "bw-store-test-" + tc.name + ".example.lan"
			tp := newStoreCert(t, dns, tc.keyArgs)
			m, err := New(Source{StoreThumbprint: tp}, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
				if got := handshake(t, m, v); got != tp {
					t.Fatalf("served %s, want %s", got, tp)
				}
			}
			byName, err := New(Source{StoreSubject: dns}, nil)
			if err != nil || byName.Info().Subject != "CN="+dns {
				t.Fatalf("by subject: %v %+v", err, byName)
			}
		})
	}
}

func TestStoreCertificateNotFound(t *testing.T) {
	if _, err := New(Source{StoreThumbprint: strings.Repeat("0", 40)}, nil); err == nil {
		t.Fatal("expected not found")
	}
}

// A certificate whose key sits in a legacy CSP is reported as such, not as
// missing.
func TestStoreCertificateLegacyCSPExplained(t *testing.T) {
	tp := newStoreCert(t, "bw-store-legacy.example.lan", "-KeyAlgorithm RSA -KeyLength 2048 -Provider 'Microsoft Enhanced RSA and AES Cryptographic Provider' -KeySpec KeyExchange")
	_, err := New(Source{StoreThumbprint: tp}, nil)
	if err == nil || !strings.Contains(err.Error(), "legacy CryptoAPI provider") {
		t.Fatalf("got %v", err)
	}
}
