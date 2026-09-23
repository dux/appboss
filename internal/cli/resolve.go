package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"dboss/internal/config"
)

type remoteOptions struct {
	json   bool
	socket string
	config string
	rest   []string
	// follow is set by `logs -f`, which polls instead of answering once.
	follow bool
}

// commonArgs pulls the flags shared by every remote command out of args, wherever they appear.
func commonArgs(args []string) (*remoteOptions, error) {
	opts := &remoteOptions{rest: make([]string, 0, len(args))}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			opts.json = true
		case "--socket", "-c", "--config":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s requires a path", args[i])
			}
			if args[i] == "--socket" {
				opts.socket = args[i+1]
			} else {
				opts.config = args[i+1]
			}
			i++
		default:
			opts.rest = append(opts.rest, args[i])
		}
	}
	return opts, nil
}

const defaultSocket = "/run/dboss/dboss.sock"

// workdir is the config in reach of a command: -c, $DBOSS_CONFIG or the current folder. It is
// read at most once, and only when the command needs it.
type workdir struct {
	explicit string
	loaded   bool
	path     string
	cfg      config.Config
	err      error
}

func (w *workdir) load() (string, config.Config, error) {
	if !w.loaded {
		w.loaded = true
		if w.path, w.err = findConfig(w.explicit); w.err == nil {
			w.cfg, w.err = config.Load(w.path)
		}
	}
	return w.path, w.cfg, w.err
}

// socket resolves the control socket: --socket, DBOSS_SOCKET, the socket of the config in reach
// if it exists on disk, then the well-known production path. The last step is what lets
// `dboss restart` inside a deployed app folder reach the host session started elsewhere.
func (w *workdir) socket(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("DBOSS_SOCKET"); env != "" {
		return env, nil
	}
	if _, err := findConfig(w.explicit); err != nil {
		return defaultSocket, nil
	}
	_, cfg, err := w.load()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(cfg.Socket); err == nil {
		return cfg.Socket, nil
	}
	return defaultSocket, nil
}

// app returns the explicit app name or, inside an app folder, that folder's app.
func (w *workdir) app(args []string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	path, cfg, err := w.load()
	if err != nil {
		return "", err
	}
	if cfg.App == nil {
		return "", fmt.Errorf("%s is a host config, name the app", path)
	}
	return filepath.Base(cfg.Dir), nil
}

// loadHostConfig loads the config file, or, when none was requested explicitly and none exists in
// the working directory, synthesizes the default host for it. That is what makes `start`, `check`
// and `doctor` work in a folder that only has an apps/ directory.
func loadHostConfig(explicit string) (config.Config, error) {
	path, err := findConfig(explicit)
	if err != nil {
		if explicit != "" || os.Getenv("DBOSS_CONFIG") != "" || !errors.Is(err, config.ErrNoConfig) {
			return config.Config{}, err
		}
		dir, wdErr := os.Getwd()
		if wdErr != nil {
			return config.Config{}, err
		}
		return config.Parse(nil, filepath.Join(dir, config.FileName))
	}
	return config.Load(path)
}

func findConfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("DBOSS_CONFIG"); env != "" {
		return env, nil
	}
	path, err := config.FindInDir(".")
	if err != nil {
		return "", fmt.Errorf("%w (use -c or DBOSS_CONFIG)", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return absolute, nil
}
