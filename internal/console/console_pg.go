package console

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"dboss/internal/config"
	"dboss/internal/metrics"
	"dboss/internal/ops"
	"dboss/internal/pg"

	"gopkg.in/yaml.v3"
)

func (h *Handler) pgStats() metrics.PGStats {
	var stats metrics.PGStats
	snapshot, err := h.service.PGSnapshot(false)
	if err != nil {
		return stats
	}
	stats.Up = snapshot.Available
	for _, database := range snapshot.Databases {
		stats.Databases = append(stats.Databases, metrics.PGDatabase{Name: database.Name, SizeBytes: database.SizeBytes})
	}
	for _, entry := range h.service.Backups() {
		moment, _ := time.Parse(time.RFC3339, entry.Time)
		stats.Backups = append(stats.Backups, metrics.PGBackup{Database: entry.Database, Time: moment, Status: entry.Status})
	}
	return stats
}

// handleHook accepts a signed ping at /hooks/<app>/<hook> and starts the hook. It is the one
// console route outside the session flow: the Git host cannot carry a session, so the hook's own
// secret is the credential. The request body is read raw for the GitHub HMAC.

func (h *Handler) writePG(w http.ResponseWriter) {
	snapshot, err := h.service.PGSnapshot(false)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot, "backups": h.service.Backups(), "backup": h.service.PGBackupConfig(), "s3": h.service.S3Configured(), "updated_at": time.Now().UTC()})
}

// pgRefresh re-inspects the server on demand. It is read-only, so it writes no audit row.
func (h *Handler) pgRefresh(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	snapshot, err := h.service.PGSnapshot(true)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot, "backups": h.service.Backups(), "backup": h.service.PGBackupConfig(), "s3": h.service.S3Configured(), "updated_at": time.Now().UTC()})
}

func (h *Handler) writePGBackups(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"backups": h.service.Backups(), "updated_at": time.Now().UTC()})
}

func (h *Handler) pgBackup(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		Database string `json:"database"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.service.Do(ops.Request{Method: ops.ActionPGBackup, Database: strings.TrimSpace(request.Database), Actor: session.Email})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "backups": h.service.Backups(), "updated_at": time.Now().UTC()})
}

func (h *Handler) pgRestore(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request pg.RestoreRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.service.Do(ops.Request{Method: ops.ActionPGRestore, BackupID: strings.TrimSpace(request.ID), Target: strings.TrimSpace(request.Target), Replace: request.Replace, Confirm: request.Confirm, Actor: session.Email})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// pgConfig writes the backup policy and database selection into the server-only host override
// and hot-reloads the PostgreSQL service, so the checkboxes apply without a daemon restart.
func (h *Handler) pgConfig(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		Backup config.PostgresBackup `json:"backup"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	host, ok := h.store.(HostConfigStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "the host config override is not available")
		return
	}
	if _, err := host.CreateHostLocal(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	file, err := h.store.Read("host")
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	contents, err := patchPostgresBackup(file.Contents, request.Backup)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	written, err := h.store.Write("host", contents, file.Revision)
	if err != nil {
		h.service.Audit(session.Email, "", "pg-config", file.ID, err)
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if cfg, err := host.HostConfig(); err == nil {
		h.service.ApplyPGConfig(cfg)
	}
	h.service.Audit(session.Email, "", "pg-config", written.ID, nil)
	snapshot, _ := h.service.PGSnapshot(false)
	writeJSON(w, http.StatusOK, map[string]any{"file": written, "snapshot": snapshot, "backups": h.service.Backups(), "backup": h.service.PGBackupConfig(), "s3": h.service.S3Configured(), "updated_at": time.Now().UTC()})
}

// patchPostgresBackup replaces the postgres.backup mapping in a host config, leaving every other
// key untouched. Comments in the file are normalized, since the override is machine-managed.
func patchPostgresBackup(contents string, backup config.PostgresBackup) (string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(contents), &root); err != nil {
		return "", err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return "", errors.New("host config must be a YAML mapping")
	}
	document := root.Content[0]
	postgres := ensureMapping(document, "postgres")
	var node yaml.Node
	if err := node.Encode(backup); err != nil {
		return "", err
	}
	setMapping(postgres, "backup", &node)
	out, err := yaml.Marshal(&root)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
