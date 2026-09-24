package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// issue makes a CA and a leaf signed by it, like an internal CA would.
func issue(t *testing.T, cn string, notAfter time.Time) (leaf *x509.Certificate, key *ecdsa.PrivateKey, ca *x509.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Issuing CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ = x509.ParseCertificate(caDER)
	key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn},
		NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: notAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ = x509.ParseCertificate(der)
	return leaf, key, ca
}

func writePEM(t *testing.T, dir string, leaf *x509.Certificate, key *ecdsa.PrivateKey, ca *x509.Certificate) (string, string) {
	t.Helper()
	cf, kf := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	chain := append(pemCert(leaf.Raw), pemCert(ca.Raw)...)
	if err := os.WriteFile(cf, chain, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kf, pemKey(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return cf, kf
}

func TestSelfSignedGeneratedOnceAndReused(t *testing.T) {
	dir := t.TempDir()
	m1, err := New(Source{SelfSigned: true, Dir: dir, Hosts: []string{"ss-test.example"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	i1 := m1.Info()
	if !i1.SelfSigned || !contains(i1.DNSNames, "ss-test.example") || !contains(i1.DNSNames, "localhost") {
		t.Fatalf("info %+v", i1)
	}
	m2, err := New(Source{SelfSigned: true, Dir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Info().NotAfter != i1.NotAfter {
		t.Fatal("self-signed certificate regenerated instead of reused")
	}
	if st, _ := os.Stat(filepath.Join(dir, "self-signed.key")); st.Mode().Perm()&0o077 != 0 && st.Mode().Perm() != 0o666 {
		t.Fatalf("key file too open: %v", st.Mode())
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestPEMChainFromCA(t *testing.T) {
	dir := t.TempDir()
	leaf, key, ca := issue(t, "ss8510.example.lan", time.Now().Add(24*time.Hour))
	cf, kf := writePEM(t, dir, leaf, key, ca)
	m, err := New(Source{CertFile: cf, KeyFile: kf}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if i := m.Info(); i.SelfSigned || !strings.Contains(i.Issuer, "Test Issuing CA") {
		t.Fatalf("info %+v", i)
	}
	if n := len(m.current().Certificate); n != 2 {
		t.Fatalf("chain length %d, want leaf + issuer", n)
	}
}

func TestRejectsExpiredAndMismatched(t *testing.T) {
	dir := t.TempDir()
	leaf, key, ca := issue(t, "old.example.lan", time.Now().Add(-time.Minute))
	cf, kf := writePEM(t, dir, leaf, key, ca)
	if _, err := New(Source{CertFile: cf, KeyFile: kf}, nil); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired accepted: %v", err)
	}
	leaf2, _, ca2 := issue(t, "a.example.lan", time.Now().Add(time.Hour))
	_, otherKey, _ := issue(t, "b.example.lan", time.Now().Add(time.Hour))
	cf, kf = writePEM(t, t.TempDir(), leaf2, otherKey, ca2)
	if _, err := New(Source{CertFile: cf, KeyFile: kf}, nil); err == nil {
		t.Fatal("mismatched key accepted")
	}
	if _, err := New(Source{}, nil); err == nil {
		t.Fatal("no source accepted")
	}
}

func TestServesAndReloadsRenewedCertificate(t *testing.T) {
	dir := t.TempDir()
	leaf, key, ca := issue(t, "old.example.lan", time.Now().Add(24*time.Hour))
	cf, kf := writePEM(t, dir, leaf, key, ca)
	var reloaded []Info
	m, err := New(Source{CertFile: cf, KeyFile: kf}, func(i Info, err error) {
		if err == nil {
			reloaded = append(reloaded, i)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	// A plain TLS listener, as the service uses (httptest would inject its
	// own certificate ahead of GetCertificate).
	ln, err := tls.Listen("tcp", "127.0.0.1:0", m.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })}
	go func() { _ = hs.Serve(ln) }()
	defer hs.Close()
	url := "https://" + ln.Addr().String()
	served := func() string {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
		resp, err := c.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.TLS.PeerCertificates[0].Subject.CommonName
	}
	if got := served(); got != "old.example.lan" {
		t.Fatalf("served %s", got)
	}
	// Renewal: new files in place, newer mtime.
	leaf2, key2, ca2 := issue(t, "new.example.lan", time.Now().Add(48*time.Hour))
	writePEM(t, dir, leaf2, key2, ca2)
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(cf, later, later)
	_ = os.Chtimes(kf, later, later)
	m.mu.Lock()
	m.lastCheck = time.Time{}
	m.mu.Unlock()
	if got := served(); got != "new.example.lan" {
		t.Fatalf("after renewal served %s", got)
	}
	if len(reloaded) != 1 || reloaded[0].Subject != "CN=new.example.lan" {
		t.Fatalf("reload callback %+v", reloaded)
	}
}

func TestSelfSignedRegeneratedWhenHostsChange(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(Source{SelfSigned: true, Dir: dir}, nil); err != nil {
		t.Fatal(err)
	}
	m, err := New(Source{SelfSigned: true, Dir: dir, Hosts: []string{"added.example.lan"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(m.Info().DNSNames, "added.example.lan") {
		t.Fatalf("new host missing: %v", m.Info().DNSNames)
	}
}

func TestBadRenewalWarnsOnce(t *testing.T) {
	dir := t.TempDir()
	leaf, key, ca := issue(t, "ok.example.lan", time.Now().Add(24*time.Hour))
	cf, kf := writePEM(t, dir, leaf, key, ca)
	var calls int
	m, err := New(Source{CertFile: cf, KeyFile: kf}, func(_ Info, err error) {
		if err != nil {
			calls++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(cf, []byte("not a certificate"), 0o644)
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(cf, later, later)
	for i := 0; i < 3; i++ {
		m.mu.Lock()
		m.lastCheck = time.Time{}
		m.mu.Unlock()
		if c := m.current(); c == nil || c.Leaf.Subject.CommonName != "ok.example.lan" {
			t.Fatal("previous certificate not kept")
		}
	}
	if calls != 1 {
		t.Fatalf("bad renewal reported %d times, want 1", calls)
	}
}

// PFX files are no longer read; the error says what to do instead.
func TestPFXRejectedWithDirections(t *testing.T) {
	pf := filepath.Join(t.TempDir(), "server.PFX")
	_ = os.WriteFile(pf, []byte("x"), 0o600)
	_, err := New(Source{CertFile: pf}, nil)
	if err == nil || !strings.Contains(err.Error(), "certutil -importpfx") {
		t.Fatalf("got %v", err)
	}
}
