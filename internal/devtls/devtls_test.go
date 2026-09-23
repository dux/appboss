package devtls

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAuthorityIsReusedAndKeepsItsKeyPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !first.root.Equal(second.root) {
		t.Fatal("a second Open made a new root")
	}
	info, err := os.Stat(filepath.Join(dir, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("root key mode = %v, want 0600", info.Mode().Perm())
	}
}

// Dev sessions started together must end up with one root, not a key from one and a
// certificate from another.
func TestConcurrentOpenCreatesOneRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	authorities := make([]*Authority, 8)
	errs := make([]error, len(authorities))
	var wait sync.WaitGroup
	for i := range authorities {
		wait.Add(1)
		go func() {
			defer wait.Done()
			authorities[i], errs[i] = Open(dir)
		}()
	}
	wait.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if !authorities[i].root.Equal(authorities[0].root) {
			t.Fatalf("open %d made its own root", i)
		}
	}
	reopened, err := Open(dir)
	if err != nil || !reopened.root.Equal(authorities[0].root) {
		t.Fatalf("root on disk differs from the one in use: %v", err)
	}
}

func TestLeafVerifiesAgainstTheRoot(t *testing.T) {
	authority, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority.root)
	for host, name := range map[string]string{"shop.lvh.me": "shop.lvh.me", "127.0.0.1": "127.0.0.1", "": "localhost"} {
		leaf, err := authority.Certificate(host)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err != nil {
			t.Errorf("%q: %v", host, err)
		}
		again, _ := authority.Certificate(host)
		if again != leaf {
			t.Errorf("%q: leaf was signed twice", host)
		}
	}
	if len(authority.leafs["shop.lvh.me"].Certificate) != 2 {
		t.Fatal("leaf does not carry the root in its chain")
	}
}
