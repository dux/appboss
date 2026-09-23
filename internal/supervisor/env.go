package supervisor

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"dboss/internal/apps"
)

func resolveExecutable(name, dir, pathValue string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		if filepath.IsAbs(name) {
			return name, nil
		}
		return filepath.Join(dir, name), nil
	}
	for _, pathDir := range filepath.SplitList(pathValue) {
		if !filepath.IsAbs(pathDir) {
			pathDir = filepath.Join(dir, pathDir)
		}
		candidate := filepath.Join(pathDir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in app PATH", name)
}

// processEnv assembles one process environment in the documented priority order, lowest first:
// the daemon environment and mise (spec.Env), then config env (extra, including a process
// override), then .env and .env.local, then the values dboss injects.
func processEnv(spec *apps.App, processName string, port int, socket string, extra map[string]string) map[string]string {
	values := map[string]string{}
	for key, value := range spec.Env {
		values[key] = value
	}
	for key, value := range extra {
		values[key] = value
	}
	for key, value := range spec.FileEnv {
		values[key] = value
	}
	// Cron jobs run outside the port table, so port 0 means no PORT is injected.
	if port > 0 {
		values["PORT"] = strconv.Itoa(port)
	}
	values["APP_NAME"] = spec.Name
	values["PROC_TYPE"] = processName
	values["DBOSS_SOCKET"] = socket
	return values
}

func envSlice(values map[string]string) []string {
	keys := slices.Sorted(maps.Keys(values))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
