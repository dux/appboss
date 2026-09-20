package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func writeSSHPub(t *testing.T, dir, name string, pub ssh.PublicKey, comment string) {
	t.Helper()
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if comment != "" {
		line += " " + comment
	}
	if err := os.WriteFile(filepath.Join(dir, name+".pub"), []byte(line+"\nComment on a second line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanSSHKeys(t *testing.T) {
	dir := t.TempDir()

	_, edPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edPublic, err := ssh.NewPublicKey(edPrivate.Public())
	if err != nil {
		t.Fatal(err)
	}
	writeSSHPub(t, dir, "zeta", edPublic, "zeta@host")
	writeSSHPub(t, dir, "alpha", edPublic, "alpha@host")

	rsaPrivate, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaPublic, err := ssh.NewPublicKey(rsaPrivate.Public())
	if err != nil {
		t.Fatal(err)
	}
	writeSSHPub(t, dir, "beta", rsaPublic, "beta@host")
	if err := os.WriteFile(filepath.Join(dir, "zeta"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.pub"), []byte("not a key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	keys, err := scanSSHKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 {
		t.Fatalf("got %d keys, want 3", len(keys))
	}
	if keys[0].Name != "alpha" || keys[1].Name != "beta" || keys[2].Name != "zeta" {
		t.Fatalf("not sorted by name: %v", keys)
	}
	if keys[0].Bits != 256 || keys[0].Type != "ssh-ed25519" {
		t.Errorf("ed25519 key = %s/%d, want ssh-ed25519/256", keys[0].Type, keys[0].Bits)
	}
	if keys[1].Bits != 2048 || keys[1].Type != "ssh-rsa" {
		t.Errorf("rsa key = %s/%d, want ssh-rsa/2048", keys[1].Type, keys[1].Bits)
	}
	if keys[0].Comment != "alpha@host" {
		t.Errorf("comment = %q, want alpha@host", keys[0].Comment)
	}
	if !keys[2].HasPrivate {
		t.Error("zeta should report a private key")
	}
	if keys[0].HasPrivate {
		t.Error("alpha has no private key on disk")
	}
	if !strings.HasPrefix(keys[0].Fingerprint, "SHA256:") {
		t.Errorf("fingerprint = %q", keys[0].Fingerprint)
	}
	if strings.Contains(keys[0].PublicKey, "second line") {
		t.Error("public key should be the first line only")
	}
}

func TestSSHKeyListCommand(t *testing.T) {
	dir := t.TempDir()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	writeSSHPub(t, dir, "id_ed25519", public, "me@host")

	var out, errOut bytes.Buffer
	if code := (CLI{In: strings.NewReader(""), Out: &out, Err: &errOut}).Run([]string{"sshkey", "list", "--dir", dir}); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "id_ed25519") || !strings.Contains(out.String(), public.Type()+" ") {
		t.Errorf("list output missing name or value:\n%s", out.String())
	}

	out.Reset()
	if code := (CLI{In: strings.NewReader(""), Out: &out, Err: &errOut}).Run([]string{"sshkey", "--dir", dir, "--json"}); code != 0 {
		t.Fatalf("default list exit = %d", code)
	}
	var keys []sshKey
	if err := json.Unmarshal(out.Bytes(), &keys); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out.String())
	}
	if len(keys) != 1 || keys[0].Name != "id_ed25519" {
		t.Fatalf("json = %+v", keys)
	}
}

func TestSSHKeyNewCommand(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	dir := t.TempDir()

	var out, errOut bytes.Buffer
	cli := CLI{In: strings.NewReader(""), Out: &out, Err: &errOut}
	if code := cli.Run([]string{"sshkey", "new", "--dir", dir, "-C", "test@host", "--no-passphrase", "mykey"}); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	for _, path := range []string{filepath.Join(dir, "mykey"), filepath.Join(dir, "mykey.pub")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s", path)
		}
	}
	if !strings.Contains(out.String(), "github.com/settings/ssh/new") {
		t.Error("output missing GitHub add URL")
	}

	keys, err := scanSSHKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "mykey" || keys[0].Comment != "test@host" {
		t.Fatalf("scanned new key = %+v", keys)
	}

	errOut.Reset()
	if code := cli.Run([]string{"sshkey", "new", "--dir", dir, "--no-passphrase", "mykey"}); code != 1 {
		t.Fatalf("overwrite without --force exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "already exists") {
		t.Errorf("missing overwrite warning: %s", errOut.String())
	}

	// the documented order puts the name before the flags
	errOut.Reset()
	if code := cli.Run([]string{"sshkey", "new", "mykey2", "--dir", dir, "--no-passphrase"}); code != 0 {
		t.Fatalf("name-first exit = %d, stderr = %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "mykey2.pub")); err != nil {
		t.Fatal("name-first key was not created in --dir")
	}
}
