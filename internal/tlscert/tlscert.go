// Package tlscert supplies the inference proxy's TLS certificate. A
// deployment uses a certificate issued by the organisation's CA (for example
// an AD CS / Microsoft CA certificate exported as PFX, or PEM files); for
// testing, a self-signed certificate is generated and kept in the data
// directory. Certificate files are re-read when they change, so a renewed
// certificate takes effect without restarting the service.
package tlscert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"software.sslmate.com/src/go-pkcs12"
)

// Source says where the certificate comes from.
type Source struct {
	// CertFile is a PEM certificate chain (leaf first) or a .pfx/.p12 file.
	CertFile string
	// KeyFile is the PEM private key; empty for PFX.
	KeyFile string
	// PFXPasswordFile holds the PFX password; empty means no password.
	PFXPasswordFile string
	// SelfSigned generates (once) and uses a self-signed certificate when
	// CertFile is empty. For testing only.
	SelfSigned bool
	// Hosts are extra DNS names or IPs for the self-signed certificate.
	Hosts []string
	// Dir holds the generated self-signed certificate.
	Dir string
	// StoreThumbprint or StoreSubject select a certificate from the Windows
	// LocalMachine\My store instead of files (the newest valid match with a
	// private key). The store is re-checked every StoreRefresh, so an AD CS
	// autoenrollment renewal is picked up without a restart.
	StoreThumbprint string
	StoreSubject    string
}

// StoreRefresh is how often a store-backed certificate is looked up again.
const StoreRefresh = 10 * time.Minute

func (s Source) fromStore() bool { return s.StoreThumbprint != "" || s.StoreSubject != "" }

// StoreCert is a certificate in the Windows store that can serve HTTPS.
type StoreCert struct {
	Thumbprint string    `json:"thumbprint"`
	Subject    string    `json:"subject"`
	Issuer     string    `json:"issuer"`
	DNSNames   []string  `json:"dns_names"`
	NotAfter   time.Time `json:"not_after"`
	SelfSigned bool      `json:"self_signed"`
}

// Info describes the certificate in use.
type Info struct {
	Subject    string    `json:"subject"`
	Issuer     string    `json:"issuer"`
	DNSNames   []string  `json:"dns_names,omitempty"`
	NotAfter   time.Time `json:"not_after"`
	SelfSigned bool      `json:"self_signed"`
	File       string    `json:"file"`
}

// Manager holds the current certificate and reloads it when its files change.
type Manager struct {
	src Source

	mu        sync.Mutex
	cert      *tls.Certificate
	info      Info
	mtimes    map[string]time.Time
	lastCheck time.Time
	onReload  func(Info, error)
}

// New loads the certificate once; an error means TLS cannot start.
func New(src Source, onReload func(Info, error)) (*Manager, error) {
	m := &Manager{src: src, onReload: onReload}
	if src.fromStore() {
		if err := m.loadStore(); err != nil {
			return nil, err
		}
		return m, nil
	}
	if src.CertFile == "" {
		if !src.SelfSigned {
			return nil, errors.New("tls: no certificate file configured and self-signed is off")
		}
		cf, kf, err := ensureSelfSigned(src.Dir, src.Hosts)
		if err != nil {
			return nil, err
		}
		m.src.CertFile, m.src.KeyFile = cf, kf
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// Info returns details of the current certificate.
func (m *Manager) Info() Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.info
}

// TLSConfig returns a server configuration that always serves the current
// certificate.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return m.current(), nil
		},
	}
}

// current returns the certificate, reloading if its files changed (checked
// at most every 30 seconds). A failed reload keeps the previous certificate.
func (m *Manager) current() *tls.Certificate {
	if m.src.fromStore() {
		return m.currentStore()
	}
	m.mu.Lock()
	check := time.Since(m.lastCheck) >= 30*time.Second
	if check {
		m.lastCheck = time.Now()
	}
	changed := check && m.filesChanged()
	m.mu.Unlock()
	if changed {
		err := m.load()
		if err != nil {
			// Keep serving the previous certificate, and do not retry (or
			// warn again) until the files change once more.
			m.mu.Lock()
			m.mtimes = m.statFiles()
			m.mu.Unlock()
		}
		if m.onReload != nil {
			m.onReload(m.Info(), err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cert
}

// currentStore re-reads the store every StoreRefresh and swaps in a newer
// certificate (for example after autoenrollment renewed it).
func (m *Manager) currentStore() *tls.Certificate {
	m.mu.Lock()
	due := time.Since(m.lastCheck) >= StoreRefresh
	if due {
		m.lastCheck = time.Now()
	}
	prev := m.info
	m.mu.Unlock()
	if due {
		err := m.loadStore()
		if err != nil || m.Info().NotAfter != prev.NotAfter || m.Info().Subject != prev.Subject {
			if m.onReload != nil {
				m.onReload(m.Info(), err)
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cert
}

func (m *Manager) loadStore() error {
	cert, err := storeCertificate(m.src.StoreThumbprint, m.src.StoreSubject)
	if err != nil {
		return err
	}
	leaf := cert.Leaf
	where := "LocalMachine\\My"
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastCheck.IsZero() {
		m.lastCheck = time.Now()
	}
	if m.cert != nil && string(m.cert.Certificate[0]) == string(cert.Certificate[0]) {
		return nil // unchanged: keep the existing signer
	}
	m.cert = cert
	m.info = Info{Subject: leaf.Subject.String(), Issuer: leaf.Issuer.String(), DNSNames: leaf.DNSNames,
		NotAfter: leaf.NotAfter, SelfSigned: leaf.Subject.String() == leaf.Issuer.String(), File: where}
	return nil
}

func (m *Manager) files() []string {
	var f []string
	for _, p := range []string{m.src.CertFile, m.src.KeyFile, m.src.PFXPasswordFile} {
		if p != "" {
			f = append(f, p)
		}
	}
	return f
}

func (m *Manager) statFiles() map[string]time.Time {
	mt := map[string]time.Time{}
	for _, p := range m.files() {
		if st, err := os.Stat(p); err == nil {
			mt[p] = st.ModTime()
		}
	}
	return mt
}

func (m *Manager) filesChanged() bool {
	for _, p := range m.files() {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !st.ModTime().Equal(m.mtimes[p]) {
			return true
		}
	}
	return false
}

func (m *Manager) load() error {
	cert, err := loadPair(m.src)
	if err != nil {
		return err
	}
	leaf := cert.Leaf
	mt := m.statFiles()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cert, m.mtimes = cert, mt
	m.info = Info{Subject: leaf.Subject.String(), Issuer: leaf.Issuer.String(), DNSNames: leaf.DNSNames,
		NotAfter: leaf.NotAfter, SelfSigned: leaf.Subject.String() == leaf.Issuer.String(), File: m.src.CertFile}
	return nil
}

func isPFX(path string) bool {
	e := strings.ToLower(filepath.Ext(path))
	return e == ".pfx" || e == ".p12"
}

func loadPair(src Source) (*tls.Certificate, error) {
	var cert tls.Certificate
	if isPFX(src.CertFile) {
		b, err := os.ReadFile(src.CertFile)
		if err != nil {
			return nil, fmt.Errorf("tls: read %s: %w", src.CertFile, err)
		}
		pw := ""
		if src.PFXPasswordFile != "" {
			p, err := os.ReadFile(src.PFXPasswordFile)
			if err != nil {
				return nil, fmt.Errorf("tls: read PFX password file: %w", err)
			}
			pw = passwordText(p)
		}
		key, leaf, chain, err := pkcs12.DecodeChain(b, pw)
		if err != nil {
			return nil, fmt.Errorf("tls: decode PFX %s: %w", src.CertFile, err)
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, errors.New("tls: PFX private key is not a signing key")
		}
		cert.PrivateKey = signer
		cert.Certificate = append(cert.Certificate, leaf.Raw)
		for _, c := range chain {
			cert.Certificate = append(cert.Certificate, c.Raw)
		}
		cert.Leaf = leaf
		// Confirm the key matches the certificate.
		if _, err := tls.X509KeyPair(pemCert(leaf.Raw), pemKey(signer)); err != nil {
			return nil, fmt.Errorf("tls: PFX key does not match certificate: %w", err)
		}
	} else {
		c, err := tls.LoadX509KeyPair(src.CertFile, src.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tls: load %s / %s: %w", src.CertFile, src.KeyFile, err)
		}
		cert = c
	}
	if cert.Leaf == nil {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, err
		}
		cert.Leaf = leaf
	}
	now := time.Now()
	if now.After(cert.Leaf.NotAfter) {
		return nil, fmt.Errorf("tls: certificate %q expired %s", cert.Leaf.Subject, cert.Leaf.NotAfter.Format(time.RFC3339))
	}
	if now.Before(cert.Leaf.NotBefore) {
		return nil, fmt.Errorf("tls: certificate %q not valid until %s", cert.Leaf.Subject, cert.Leaf.NotBefore.Format(time.RFC3339))
	}
	return &cert, nil
}

// passwordText decodes a password file saved as ASCII/UTF-8 (with or
// without a byte-order mark) or UTF-16 (Windows PowerShell 5.1's default for
// Out-File), and drops trailing line breaks.
func passwordText(b []byte) string {
	switch {
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE: // UTF-16 LE
		u := make([]uint16, 0, len(b)/2)
		for i := 2; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		return strings.TrimRight(string(utf16.Decode(u)), "\r\n")
	case len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF:
		b = b[3:]
	}
	return strings.TrimRight(string(b), "\r\n")
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pemKey(k crypto.Signer) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// covers reports whether the certificate names every host.
func covers(c *x509.Certificate, hosts []string) bool {
	for _, h := range hosts {
		if h != "" && c.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

// ensureSelfSigned creates dir/self-signed.{crt,key} if missing and returns
// their paths. The certificate lasts one year and covers localhost, the
// loopback addresses, this machine's host name, and hosts.
func ensureSelfSigned(dir string, hosts []string) (string, string, error) {
	cf, kf := filepath.Join(dir, "self-signed.crt"), filepath.Join(dir, "self-signed.key")
	if c, err := loadPair(Source{CertFile: cf, KeyFile: kf}); err == nil && time.Until(c.Leaf.NotAfter) > 7*24*time.Hour && covers(c.Leaf, hosts) {
		return cf, kf, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	hn, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Benchwarmer self-signed (testing only)"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range append([]string{"localhost", "127.0.0.1", "::1", hn}, hosts...) {
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(kf, pemKey(key), 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(cf, pemCert(der), 0o644); err != nil {
		return "", "", err
	}
	return cf, kf, nil
}
