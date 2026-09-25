package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"dboss/internal/release"
	"dboss/internal/version"
)

type updateResult struct {
	Current string `json:"current"`
	Latest  string `json:"latest"`
	Binary  string `json:"binary"`
	Updated bool   `json:"updated"`
}

// update replaces this binary with a GitHub release asset: resolve a tag, download the asset
// for this platform, check its sha256 and rename it over the running executable. It is the Go
// port of install.sh's download half, with none of the host setup.
func (c CLI) update(args []string) error {
	set := flag.NewFlagSet("update", flag.ContinueOnError)
	set.SetOutput(c.Err)
	check := set.Bool("check", false, "report whether a newer release exists, then exit")
	force := set.Bool("force", false, "install even over a from-source build or the same version")
	wanted := set.String("version", "", "release tag to install (default: the latest release)")
	jsonOutput := set.Bool("json", false, "JSON output")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: dboss update [--check] [--version tag] [--force]")
	}

	target, err := executablePath()
	if err != nil {
		return err
	}
	current := version.String()
	// Refuse a from-source build before the network call, so the answer to `dboss update` in a
	// checkout is immediate and names the command that actually updates it.
	if !*check && !*force && current == version.Dev {
		return fmt.Errorf("this is a from-source build (%s): rebuild it with `make build`, or pass --force to replace %s with the latest release", version.Dev, target)
	}

	tag := *wanted
	if tag == "" {
		if tag, err = release.Latest(context.Background()); err != nil {
			return err
		}
	}

	// The displayed version is dotted (v0.8.4); compare the raw injected count instead.
	latest, haveLatest := release.Number(tag)
	running, haveRunning := release.Number(version.Version)
	comparable := haveLatest && haveRunning
	newer := comparable && latest > running
	install := !*check && (!comparable || newer || *force)

	if !*jsonOutput {
		switch {
		case !comparable:
			fmt.Fprintf(c.Out, "latest release is %s, this build reports %s\n", tag, current)
		case newer:
			fmt.Fprintf(c.Out, "latest release is %s, you are on %s\n", tag, current)
		default:
			fmt.Fprintf(c.Out, "dboss %s is the latest release\n", current)
		}
	}
	if install {
		if err := c.installRelease(target, tag, *jsonOutput); err != nil {
			return err
		}
	}
	if *jsonOutput {
		encoded, _ := json.MarshalIndent(updateResult{Current: current, Latest: tag, Binary: target, Updated: install}, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	if install {
		fmt.Fprintf(c.Out, "updated %s to %s\n", target, tag)
		fmt.Fprintln(c.Out, "restart the host to run it: sudo systemctl restart dboss")
	}
	return nil
}

// installRelease stages the asset next to the running binary, so the final move is a rename
// on one filesystem rather than a copy across devices.
func (c CLI) installRelease(target, tag string, quiet bool) error {
	asset := "dboss_" + runtime.GOOS + "_" + runtime.GOARCH

	dir, err := os.MkdirTemp(filepath.Dir(target), ".dboss-update-")
	if err != nil {
		return updateWriteError(filepath.Dir(target), err)
	}
	defer os.RemoveAll(dir)

	sums := filepath.Join(dir, "checksums.txt")
	if _, err := download(release.AssetURL(tag, "checksums.txt"), sums); err != nil {
		return err
	}
	listing, err := os.ReadFile(sums)
	if err != nil {
		return err
	}
	expected := checksumFor(string(listing), asset)
	if expected == "" {
		return fmt.Errorf("release %s has no checksum for %s", tag, asset)
	}

	if !quiet {
		fmt.Fprintf(c.Out, "downloading %s ... ", asset)
	}
	staged := filepath.Join(dir, asset)
	actual, err := download(release.AssetURL(tag, asset), staged)
	if err != nil {
		if !quiet {
			fmt.Fprintln(c.Out)
		}
		return err
	}
	if actual != expected {
		if !quiet {
			fmt.Fprintln(c.Out)
		}
		return fmt.Errorf("checksum mismatch for %s, the binary was left alone:\n  expected %s\n  actual   %s", asset, expected, actual)
	}
	if !quiet {
		fmt.Fprintln(c.Out, "sha256 ok")
	}

	// A running executable cannot be written to, but it can be replaced.
	if err := os.Rename(staged, target); err != nil {
		return updateWriteError(target, err)
	}
	return nil
}

// executablePath resolves the binary behind any symlink, so `dboss update` replaces the real
// file rather than the link that found it.
func executablePath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate this binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	return resolved, nil
}

// download streams url into path and returns the sha256 of what it wrote.
func download(url, path string) (string, error) {
	response, err := release.Get(context.Background(), url)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hasher), response.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return "", fmt.Errorf("download %s: %w", url, copyErr)
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// checksumFor reads one asset's hash out of a `sha256sum` listing.
func checksumFor(listing, asset string) string {
	for line := range strings.SplitSeq(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			return fields[0]
		}
	}
	return ""
}

// updateWriteError turns a permission failure into the one instruction that fixes it.
func updateWriteError(path string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%s is not writable: re-run with `sudo dboss update`", path)
	}
	return err
}
