// Package devtls is the built-in certificate authority behind HTTPS in a dev session. One root
// per user is created on first use and shared by every project; leaf certificates are signed in
// memory for whatever host the browser asks for, so there is nothing to renew. A host session
// never uses it: production TLS is ACME through proxy.tls.
package devtls

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
	"syscall"
	"time"

	"dboss/internal/fsutil"
)

const (
	rootFile = "root.pem"
	keyFile  = "root-key.pem"
	// Apple rejects a leaf from a user-installed root that lives longer than 825 days; a leaf
	// here lives for one session anyway.
	leafLifetime = 397 * 24 * time.Hour
	rootLifetime = 10 * 365 * 24 * time.Hour
)

// Authority signs dev certificates with the user's root.
type Authority struct {
	dir   string
	root  *x509.Certificate
	key   crypto.Signer
	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

// DefaultDir is where the root lives: the user's config directory, so every project shares it.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "dboss", "ca"), nil
}

// Open loads the root from dir, creating it on first use. Dev sessions in other app folders may
// start at the same moment, so the check and the create run under a lock on the directory.
func Open(dir string) (*Authority, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	a := &Authority{dir: dir, leafs: map[string]*tls.Certificate{}}
	certPEM, certErr := os.ReadFile(a.RootPath())
	keyPEM, keyErr := os.ReadFile(filepath.Join(dir, keyFile))
	if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
		if err := a.create(); err != nil {
			return nil, err
		}
		return a, nil
	}
	if certErr != nil {
		return nil, certErr
	}
	if keyErr != nil {
		return nil, keyErr
	}
	if err := a.load(certPEM, keyPEM); err != nil {
		return nil, fmt.Errorf("dev certificate authority in %s: %w", dir, err)
	}
	return a, nil
}

// RootPath is the root certificate, the one file a trust store needs.
func (a *Authority) RootPath() string { return filepath.Join(a.dir, rootFile) }

func (a *Authority) create() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	template := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{Organization: []string{"dboss development CA"}, CommonName: "dboss dev root " + host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(rootLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFile(filepath.Join(a.dir, keyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := fsutil.WriteFile(a.RootPath(), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return a.load(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func lockDir(dir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func (a *Authority) load(certPEM, keyPEM []byte) error {
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return errors.New("unreadable PEM")
	}
	root, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return err
	}
	key, ok := parsed.(crypto.Signer)
	if !ok {
		return errors.New("root key cannot sign")
	}
	a.root, a.key = root, key
	return nil
}

// TLSConfig serves a certificate for the requested host, signed on first use.
func (a *Authority) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return a.Certificate(hello.ServerName)
		},
	}
}

// Certificate returns the leaf for host. A request without SNI (an IP in the URL) gets the
// loopback leaf, which names localhost and both loopback addresses.
func (a *Authority) Certificate(host string) (*tls.Certificate, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	a.mu.Lock()
	defer a.mu.Unlock()
	if leaf := a.leafs[host]; leaf != nil {
		return leaf, nil
	}
	leaf, err := a.sign(host)
	if err != nil {
		return nil, err
	}
	a.leafs[host] = leaf
	return leaf, nil
}

func (a *Authority) sign(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{Organization: []string{"dboss development certificate"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	switch {
	case host == "":
		template.DNSNames = []string{"localhost"}
		template.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	case net.ParseIP(host) != nil:
		template.IPAddresses = []net.IP{net.ParseIP(host)}
	default:
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.root, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der, a.root.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

// Trusted reports whether the system trust store accepts a leaf from this root, which is what a
// browser will decide too. On macOS this asks the platform verifier, so a root added to the
// login keychain counts.
func (a *Authority) Trusted() bool {
	leaf, err := a.Certificate("localhost")
	if err != nil {
		return false
	}
	_, err = leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "localhost"})
	return err == nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
