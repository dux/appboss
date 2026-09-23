package ports

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"dboss/internal/fsutil"
)

const lockFile = "sessions.lock"

// Registry is the machine-wide ledger of dev sessions: each live session keeps a file listing
// the ports it holds, so `dboss start` in several app folders never hands out the same port twice
// or kills another session's listeners. Claims serialize on one lock file, and a session that
// died without closing is dropped by the next claim.
type Registry struct {
	dir    string
	file   string
	state  string // this folder's ports.json: what its last run held, keyed app/process
	window [2]int

	mu       sync.Mutex
	previous map[string]int
	claimed  map[string]int
}

type session struct {
	PID   int            `json:"pid"`
	Ports map[string]int `json:"ports"`
}

// DefaultSessionsDir is the registry every dev session of this user shares.
func DefaultSessionsDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "dboss", "sessions"), nil
}

// OpenRegistry joins the registry in dir. state is the folder's own record of the ports it held
// last time, preferred again when free so URLs survive a restart; window bounds every claim.
func OpenRegistry(dir, state string, window [2]int) (*Registry, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	previous := map[string]int{}
	if err := fsutil.ReadJSON(state, &previous); err != nil {
		return nil, fmt.Errorf("read %s: %w", state, err)
	}
	id := make([]byte, 4)
	_, _ = rand.Read(id)
	file := filepath.Join(dir, strconv.Itoa(os.Getpid())+"-"+hex.EncodeToString(id)+".json")
	return &Registry{dir: dir, file: file, state: state, window: window, previous: previous, claimed: map[string]int{}}, nil
}

// Stale lists the ports this folder held last time that no live session holds now. Whatever
// still listens there was left behind by a run of this folder that did not shut down.
func (r *Registry) Stale() ([]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	unlock, err := r.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	held, err := r.held()
	if err != nil {
		return nil, err
	}
	var stale []int
	for _, port := range r.previous {
		if !held[port] {
			stale = append(stale, port)
		}
	}
	sort.Ints(stale)
	return stale, nil
}

// Claim hands key (app/process) a port no live session holds and nothing listens on: the one
// key had last time when it is still free, else the lowest free port of the window.
func (r *Registry) Claim(key string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if port, ok := r.claimed[key]; ok {
		return port, nil
	}
	unlock, err := r.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	held, err := r.held()
	if err != nil {
		return 0, err
	}
	for _, port := range r.claimed {
		held[port] = true
	}
	usable := func(port int) bool {
		return port >= r.window[0] && port <= r.window[1] && !held[port] && Free(port)
	}
	port := r.previous[key]
	if port == 0 || !usable(port) {
		port = 0
		for candidate := r.window[0]; candidate <= r.window[1]; candidate++ {
			if usable(candidate) {
				port = candidate
				break
			}
		}
	}
	if port == 0 {
		return 0, fmt.Errorf("no free port in %d-%d", r.window[0], r.window[1])
	}
	r.claimed[key] = port
	r.previous[key] = port
	if err := fsutil.WriteJSON(r.file, session{PID: os.Getpid(), Ports: r.claimed}, 0o600); err != nil {
		return 0, err
	}
	if err := fsutil.WriteJSON(r.state, r.previous, 0o640); err != nil {
		return 0, err
	}
	return port, nil
}

// Recorded is every port a folder's state file names: what its dev session holds, or held last.
func Recorded(state string) ([]int, error) {
	recorded := map[string]int{}
	if err := fsutil.ReadJSON(state, &recorded); err != nil {
		return nil, err
	}
	var result []int
	for _, port := range recorded {
		result = append(result, port)
	}
	sort.Ints(result)
	return result, nil
}

// Close gives every claimed port back.
func (r *Registry) Close() error {
	err := os.Remove(r.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (r *Registry) lock() (func(), error) {
	file, err := os.OpenFile(filepath.Join(r.dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
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

// held is every port the other live sessions hold. It runs under the lock, so a dead session's
// file can be removed here without racing a claim.
func (r *Registry) held() (map[int]bool, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, err
	}
	held := map[int]bool{}
	for _, entry := range entries {
		path := filepath.Join(r.dir, entry.Name())
		if !strings.HasSuffix(entry.Name(), ".json") || path == r.file {
			continue
		}
		var other session
		if err := fsutil.ReadJSON(path, &other); err != nil || !alive(other.PID) {
			_ = os.Remove(path)
			continue
		}
		for _, port := range other.Ports {
			held[port] = true
		}
	}
	return held, nil
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Free reports whether nothing listens on port. A wildcard bind can succeed next to an app bound
// only to a loopback address, so both loopback addresses are dialed as well.
func Free(port int) bool {
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	for _, host := range []string{"127.0.0.1", "::1"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return false
		}
	}
	return true
}
