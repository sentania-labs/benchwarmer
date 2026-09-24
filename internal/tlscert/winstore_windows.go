//go:build windows

package tlscert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modncrypt            = windows.NewLazySystemDLL("ncrypt.dll")
	procNCryptSignHash   = modncrypt.NewProc("NCryptSignHash")
	procNCryptFreeObject = modncrypt.NewProc("NCryptFreeObject")
)

const (
	bcryptPadPKCS1 = 0x2
	bcryptPadPSS   = 0x8
)

// storeCertificate finds the newest currently valid certificate with a
// private key in LocalMachine\My that matches the thumbprint (hex SHA-1) or,
// when thumbprint is empty, whose subject or DNS names contain subject. The
// private key stays in Windows: signing goes through CNG, so a
// non-exportable key works.
func storeCertificate(thumbprint, subject string) (*tls.Certificate, error) {
	name, _ := windows.UTF16PtrFromString("MY")
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		windows.CERT_SYSTEM_STORE_LOCAL_MACHINE|windows.CERT_STORE_READONLY_FLAG, uintptr(unsafe.Pointer(name)))
	if err != nil {
		return nil, fmt.Errorf("tls: open LocalMachine\\My: %w", err)
	}
	defer windows.CertCloseStore(store, 0)

	want := strings.ToLower(strings.ReplaceAll(thumbprint, " ", ""))
	now := time.Now()
	var best *windows.CertContext
	var bestLeaf *x509.Certificate
	var prev *windows.CertContext
	for {
		ctx, err := windows.CertFindCertificateInStore(store, windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING, 0,
			windows.CERT_FIND_ANY, nil, prev)
		if err != nil {
			break // CRYPT_E_NOT_FOUND ends the enumeration and frees prev
		}
		prev = ctx
		// Copy: x509.ParseCertificate keeps slices of its input, and this
		// memory belongs to Windows and is freed with the context.
		der := append([]byte(nil), unsafe.Slice(ctx.EncodedCert, ctx.Length)...)
		leaf, err := x509.ParseCertificate(der)
		if err != nil || now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			continue
		}
		if want != "" {
			sum := sha1.Sum(der)
			if hex.EncodeToString(sum[:]) != want {
				continue
			}
		} else if !matchesSubject(leaf, subject) {
			continue
		}
		if !usableForServer(leaf) {
			continue
		}
		if !hasPrivateKey(ctx) {
			continue
		}
		if bestLeaf == nil || leaf.NotAfter.After(bestLeaf.NotAfter) {
			if best != nil {
				windows.CertFreeCertificateContext(best)
			}
			best, bestLeaf = windows.CertDuplicateCertificateContext(ctx), leaf
		}
	}
	if best == nil {
		if want != "" {
			return nil, fmt.Errorf("tls: no valid certificate with a private key and thumbprint %s in LocalMachine\\My", thumbprint)
		}
		return nil, fmt.Errorf("tls: no valid certificate with a private key matching %q in LocalMachine\\My", subject)
	}
	// The signer owns best from here: a cached CNG key handle is only valid
	// while its certificate context lives.
	signer, err := newNCryptSigner(best, bestLeaf.PublicKey)
	if err != nil {
		windows.CertFreeCertificateContext(best)
		return nil, err
	}
	chain := [][]byte{bestLeaf.Raw}
	chain = append(chain, intermediates(best)...)
	return &tls.Certificate{Certificate: chain, PrivateKey: signer, Leaf: bestLeaf}, nil
}

// ListStore describes the certificates in LocalMachine\My that could serve
// the inference listener: currently valid, allowed for server
// authentication, and with a private key. It feeds the dashboard's picker.
func ListStore() ([]StoreCert, error) {
	name, _ := windows.UTF16PtrFromString("MY")
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		windows.CERT_SYSTEM_STORE_LOCAL_MACHINE|windows.CERT_STORE_READONLY_FLAG, uintptr(unsafe.Pointer(name)))
	if err != nil {
		return nil, fmt.Errorf("tls: open LocalMachine\\My: %w", err)
	}
	defer windows.CertCloseStore(store, 0)
	now := time.Now()
	out := []StoreCert{}
	var prev *windows.CertContext
	for {
		ctx, err := windows.CertFindCertificateInStore(store, windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING, 0,
			windows.CERT_FIND_ANY, nil, prev)
		if err != nil {
			break // CRYPT_E_NOT_FOUND ends the enumeration and frees prev
		}
		prev = ctx
		der := append([]byte(nil), unsafe.Slice(ctx.EncodedCert, ctx.Length)...)
		leaf, err := x509.ParseCertificate(der)
		if err != nil || now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) || !usableForServer(leaf) || !hasPrivateKey(ctx) {
			continue
		}
		sum := sha1.Sum(der)
		out = append(out, StoreCert{Thumbprint: strings.ToUpper(hex.EncodeToString(sum[:])), Subject: leaf.Subject.String(),
			Issuer: leaf.Issuer.String(), DNSNames: leaf.DNSNames, NotAfter: leaf.NotAfter,
			SelfSigned: leaf.Subject.String() == leaf.Issuer.String()})
	}
	return out, nil
}

// matchesSubject accepts a DNS name the certificate is valid for (including
// through a wildcard) or, failing that, a substring of the subject DN.
func matchesSubject(c *x509.Certificate, s string) bool {
	if s == "" {
		return false
	}
	if c.VerifyHostname(s) == nil {
		return true
	}
	return strings.Contains(strings.ToLower(c.Subject.String()), strings.ToLower(s))
}

// usableForServer rejects certificates that clients would refuse for a
// TLS server, such as an autoenrolled workstation (client-auth) certificate.
func usableForServer(c *x509.Certificate) bool {
	if c.KeyUsage != 0 && c.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return false
	}
	if len(c.ExtKeyUsage) == 0 && len(c.UnknownExtKeyUsage) == 0 {
		return true
	}
	for _, u := range c.ExtKeyUsage {
		if u == x509.ExtKeyUsageServerAuth || u == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

func hasPrivateKey(ctx *windows.CertContext) bool {
	var key windows.Handle
	var spec uint32
	var mustFree bool
	err := windows.CryptAcquireCertificatePrivateKey(ctx,
		windows.CRYPT_ACQUIRE_ONLY_NCRYPT_KEY_FLAG|windows.CRYPT_ACQUIRE_SILENT_FLAG, nil, &key, &spec, &mustFree)
	if err != nil {
		return false
	}
	if mustFree {
		procNCryptFreeObject.Call(uintptr(key))
	}
	return true
}

// intermediates returns the issuing certificates between the leaf and the
// root, as built by Windows from the machine's stores.
func intermediates(ctx *windows.CertContext) [][]byte {
	var chainCtx *windows.CertChainContext
	para := windows.CertChainPara{Size: uint32(unsafe.Sizeof(windows.CertChainPara{}))}
	// Cache-only URL retrieval: never block a TLS handshake on the network
	// fetching missing issuers.
	const certChainCacheOnlyURLRetrieval = 0x00000004
	if err := windows.CertGetCertificateChain(0, ctx, nil, 0, &para, certChainCacheOnlyURLRetrieval, 0, &chainCtx); err != nil {
		return nil
	}
	defer windows.CertFreeCertificateChain(chainCtx)
	if chainCtx.ChainCount == 0 {
		return nil
	}
	simple := unsafe.Slice(chainCtx.Chains, chainCtx.ChainCount)[0]
	elems := unsafe.Slice(simple.Elements, simple.NumElements)
	var out [][]byte
	// Element 0 is the leaf. The last element is sent too unless it is a
	// self-signed root (clients must already trust roots); on an incomplete
	// chain the last element is an intermediate the client needs.
	for i := 1; i < len(elems); i++ {
		c := elems[i].CertContext
		der := append([]byte(nil), unsafe.Slice(c.EncodedCert, c.Length)...)
		if i == len(elems)-1 {
			if p, err := x509.ParseCertificate(der); err == nil && p.CheckSignatureFrom(p) == nil {
				break
			}
		}
		out = append(out, der)
	}
	return out
}

// ncryptSigner signs with a CNG key held by Windows.
type ncryptSigner struct {
	mu      sync.Mutex
	key     windows.Handle
	freeKey bool
	ctx     *windows.CertContext // kept alive for the key handle
	pub     crypto.PublicKey
}

func newNCryptSigner(ctx *windows.CertContext, pub crypto.PublicKey) (*ncryptSigner, error) {
	var key windows.Handle
	var spec uint32
	var mustFree bool
	if err := windows.CryptAcquireCertificatePrivateKey(ctx,
		windows.CRYPT_ACQUIRE_ONLY_NCRYPT_KEY_FLAG|windows.CRYPT_ACQUIRE_SILENT_FLAG, nil, &key, &spec, &mustFree); err != nil {
		return nil, fmt.Errorf("tls: acquire private key: %w", err)
	}
	if spec != windows.CERT_NCRYPT_KEY_SPEC {
		if mustFree {
			procNCryptFreeObject.Call(uintptr(key))
		}
		return nil, errors.New("tls: private key is not a CNG key")
	}
	s := &ncryptSigner{key: key, freeKey: mustFree, ctx: ctx, pub: pub}
	runtime.SetFinalizer(s, func(s *ncryptSigner) {
		if s.freeKey {
			procNCryptFreeObject.Call(uintptr(s.key))
		}
		windows.CertFreeCertificateContext(s.ctx)
	})
	return s, nil
}

func (s *ncryptSigner) Public() crypto.PublicKey { return s.pub }

func hashAlgID(h crypto.Hash) (*uint16, error) {
	name := map[crypto.Hash]string{crypto.SHA1: "SHA1", crypto.SHA256: "SHA256", crypto.SHA384: "SHA384", crypto.SHA512: "SHA512"}[h]
	if name == "" {
		return nil, fmt.Errorf("tls: unsupported hash %v", h)
	}
	return windows.UTF16PtrFromString(name)
}

// Sign implements crypto.Signer for RSA (PKCS#1 v1.5 and PSS) and ECDSA.
func (s *ncryptSigner) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var padding unsafe.Pointer
	var flags uint32
	switch s.pub.(type) {
	case *rsa.PublicKey:
		alg, err := hashAlgID(opts.HashFunc())
		if err != nil {
			return nil, err
		}
		if pss, ok := opts.(*rsa.PSSOptions); ok {
			salt := pss.SaltLength
			if salt == rsa.PSSSaltLengthEqualsHash || salt == rsa.PSSSaltLengthAuto {
				salt = opts.HashFunc().Size()
			}
			info := &struct {
				AlgID *uint16
				Salt  uint32
			}{alg, uint32(salt)}
			padding, flags = unsafe.Pointer(info), bcryptPadPSS
		} else {
			info := &struct{ AlgID *uint16 }{alg}
			padding, flags = unsafe.Pointer(info), bcryptPadPKCS1
		}
	case *ecdsa.PublicKey:
	default:
		return nil, fmt.Errorf("tls: unsupported key type %T", s.pub)
	}
	// Never prompt: a key that wants UI would block every handshake.
	const ncryptSilentFlag = 0x40
	flags |= ncryptSilentFlag
	var size uint32
	r, _, _ := procNCryptSignHash.Call(uintptr(s.key), uintptr(padding), uintptr(unsafe.Pointer(&digest[0])), uintptr(len(digest)),
		0, 0, uintptr(unsafe.Pointer(&size)), uintptr(flags))
	if r != 0 {
		return nil, fmt.Errorf("tls: NCryptSignHash size: 0x%x", r)
	}
	sig := make([]byte, size)
	r, _, _ = procNCryptSignHash.Call(uintptr(s.key), uintptr(padding), uintptr(unsafe.Pointer(&digest[0])), uintptr(len(digest)),
		uintptr(unsafe.Pointer(&sig[0])), uintptr(size), uintptr(unsafe.Pointer(&size)), uintptr(flags))
	if r != 0 {
		return nil, fmt.Errorf("tls: NCryptSignHash: 0x%x", r)
	}
	sig = sig[:size]
	if _, ok := s.pub.(*ecdsa.PublicKey); ok {
		// CNG returns r||s; TLS wants an ASN.1 ECDSA-Sig-Value.
		half := len(sig) / 2
		return asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(sig[:half]), new(big.Int).SetBytes(sig[half:])})
	}
	return sig, nil
}
