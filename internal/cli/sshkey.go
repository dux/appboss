package cli

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// sshKey is one public key found on disk.
type sshKey struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	PublicKey   string `json:"publicKey"`
	Type        string `json:"type"`
	Bits        int    `json:"bits"`
	Fingerprint string `json:"fingerprint"`
	Comment     string `json:"comment"`
	HasPrivate  bool   `json:"hasPrivate"`
}

// sshkey lists the local public keys, or creates a new key pair with ssh-keygen.
func (c CLI) sshkey(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return c.sshkeyList(args)
	}
	switch args[0] {
	case "list", "ls":
		return c.sshkeyList(args[1:])
	case "new", "create", "gen":
		return c.sshkeyNew(args[1:])
	case "help":
		return c.help(c.Out, "sshkey")
	default:
		return fmt.Errorf("unknown sshkey subcommand %q (use list or new)", args[0])
	}
}

func (c CLI) sshkeyList(args []string) error {
	set := flag.NewFlagSet("sshkey list", flag.ContinueOnError)
	set.SetOutput(c.Err)
	dir := set.String("dir", sshDir(), "directory to scan")
	asJSON := set.Bool("json", false, "machine-readable output")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: dboss sshkey list [--dir path] [--json]")
	}

	keys, err := scanSSHKeys(*dir)
	if err != nil {
		return err
	}
	if *asJSON {
		encoded, err := json.MarshalIndent(keys, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	if len(keys) == 0 {
		fmt.Fprintf(c.Out, "no public keys in %s\n", *dir)
		return nil
	}
	width := 0
	for _, key := range keys {
		width = max(width, len(key.Name))
	}
	for _, key := range keys {
		fmt.Fprintf(c.Out, "%-*s  %s\n", width, key.Name, key.PublicKey)
	}
	return nil
}

// sshkeyNew creates a key with ssh-keygen, defaulting to ed25519, and prints the public value
// plus the GitHub and GitLab add pages.
func (c CLI) sshkeyNew(args []string) error {
	set := flag.NewFlagSet("sshkey new", flag.ContinueOnError)
	set.SetOutput(c.Err)
	dir := set.String("dir", sshDir(), "directory for the new key")
	keyType := set.String("t", "ed25519", "key type: ed25519, rsa or ecdsa")
	bits := set.Int("b", 0, "key bits (default 4096 rsa, 521 ecdsa)")
	comment := set.String("C", "", "key comment (default user@host)")
	target := set.String("f", "", "target path (default dir/name)")
	noPass := set.Bool("no-passphrase", false, "create without a passphrase")
	force := set.Bool("force", false, "overwrite an existing key")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() > 1 {
		return errors.New("usage: dboss sshkey new [name]")
	}

	kind := strings.ToLower(*keyType)
	switch kind {
	case "ed25519", "rsa", "ecdsa":
	default:
		return fmt.Errorf("unknown key type %q (want ed25519, rsa or ecdsa)", *keyType)
	}

	path := *target
	if path == "" {
		name := set.Arg(0)
		if name == "" {
			name = defaultSSHKeyName(kind)
		}
		path = filepath.Join(*dir, name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		if !*force {
			return fmt.Errorf("%s already exists (use --force to overwrite)", path)
		}
		os.Remove(path)
		os.Remove(path + ".pub")
	}

	genBits := *bits
	if genBits == 0 {
		switch kind {
		case "rsa":
			genBits = 4096
		case "ecdsa":
			genBits = 521
		}
	}
	genComment := *comment
	if genComment == "" {
		genComment = defaultSSHComment()
	}

	genArgs := []string{"-t", kind, "-f", path, "-C", genComment}
	if genBits > 0 {
		genArgs = append(genArgs, "-b", strconv.Itoa(genBits))
	}
	if *noPass {
		genArgs = append(genArgs, "-N", "")
	}
	command := exec.Command("ssh-keygen", genArgs...)
	command.Stdin = c.In
	command.Stdout = c.Out
	command.Stderr = c.Err
	if err := command.Run(); err != nil {
		return fmt.Errorf("ssh-keygen: %w", err)
	}
	os.Chmod(path, 0o600)
	os.Chmod(path+".pub", 0o644)

	data, err := os.ReadFile(path + ".pub")
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "\nCreated %s key: %s\n\n  %s  %s\n\nAdd it to:\n", kind, path, filepath.Base(path), firstLine(string(data)))
	fmt.Fprintln(c.Out, "  GitHub  https://github.com/settings/ssh/new")
	fmt.Fprintln(c.Out, "  GitLab  https://gitlab.com/-/user_settings/ssh_keys")
	return nil
}

// scanSSHKeys reads every *.pub in dir. Malformed or unreadable files are skipped.
func scanSSHKeys(dir string) ([]sshKey, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	keys := make([]sshKey, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pub") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		line := firstLine(string(data))
		pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".pub")
		_, statErr := os.Stat(filepath.Join(dir, name))
		keys = append(keys, sshKey{
			Name:        name,
			Path:        filepath.Join(dir, entry.Name()),
			PublicKey:   line,
			Type:        pub.Type(),
			Bits:        sshKeyBits(pub),
			Fingerprint: ssh.FingerprintSHA256(pub),
			Comment:     comment,
			HasPrivate:  statErr == nil,
		})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	return keys, nil
}

func sshKeyBits(pub ssh.PublicKey) int {
	cp, ok := pub.(ssh.CryptoPublicKey)
	if !ok {
		return 0
	}
	switch key := cp.CryptoPublicKey().(type) {
	case *rsa.PublicKey:
		return key.N.BitLen()
	case *ecdsa.PublicKey:
		return key.Curve.Params().BitSize
	case ed25519.PublicKey:
		return 256
	}
	return 0
}

func sshDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ssh"
	}
	return filepath.Join(home, ".ssh")
}

func defaultSSHKeyName(kind string) string {
	switch kind {
	case "rsa":
		return "id_rsa"
	case "ecdsa":
		return "id_ecdsa"
	default:
		return "id_ed25519"
	}
}

func defaultSSHComment() string {
	name := os.Getenv("USER")
	if name == "" {
		if current, err := user.Current(); err == nil {
			name = current.Username
		}
	}
	host, _ := os.Hostname()
	switch {
	case name == "":
		return host
	case host == "":
		return name
	default:
		return name + "@" + host
	}
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = strings.TrimSpace(text[:index])
	}
	return text
}
