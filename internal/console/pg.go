package console

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/ops"
	"dboss/internal/pg"

	"gopkg.in/yaml.v3"
)

func (h *Handler) writePG(w http.ResponseWriter, _ *http.Request) {
	snapshot, err := h.service.PGSnapshot(false)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot, "backups": h.service.Backups(), "rotation": h.service.PGBackupConfig(), "updated_at": time.Now().UTC()})
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
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot, "backups": h.service.Backups(), "rotation": h.service.PGBackupConfig(), "updated_at": time.Now().UTC()})
}

func (h *Handler) writePGBackups(w http.ResponseWriter, _ *http.Request) {
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

// pgDeleteBackup removes one recorded dump from disk and the catalog.
func (h *Handler) pgDeleteBackup(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := h.service.Do(ops.Request{Method: ops.ActionPGDeleteDump, BackupID: strings.TrimSpace(request.ID), Actor: session.Email}); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": h.service.Backups(), "updated_at": time.Now().UTC()})
}

// pgDownloadBackup streams one recorded dump exactly as it sits on disk, so the archive can be
// carried to another host and uploaded there. It moves a full copy of a database off the box, so
// it audits like a mutating action.
func (h *Handler) pgDownloadBackup(w http.ResponseWriter, r *http.Request, session authSession) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	entry, path, err := h.service.BackupFile(id)
	if err != nil {
		h.service.Audit(session.Email, "", "pg-download-dump", id, err)
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	h.service.Audit(session.Email, "", "pg-download-dump", entry.ID, nil)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+entry.Database+"_"+entry.ID+"\"")
	http.ServeFile(w, r, path)
}

// pgUploadBackup stores an archive the operator picked for ?database=<name>. The file streams
// straight to disk: a dump is far larger than any JSON body this console handles.
func (h *Handler) pgUploadBackup(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	database := strings.TrimSpace(r.URL.Query().Get("database"))
	r.Body = http.MaxBytesReader(w, r.Body, maxBackupUpload)
	parts, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "upload must be a multipart form")
		return
	}
	for {
		part, err := parts.NextPart()
		if err != nil {
			writeError(w, http.StatusBadRequest, "upload is missing its file")
			return
		}
		if part.FileName() == "" {
			_ = part.Close()
			continue
		}
		entry, err := h.service.ImportBackup(database, part)
		_ = part.Close()
		h.service.Audit(session.Email, "", "pg-upload-dump", database, err)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"backup": entry, "backups": h.service.Backups(), "updated_at": time.Now().UTC()})
		return
	}
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

// pgDrop removes a database. The operator types its name as confirmation, which travels as the
// confirm field and is checked by the service.
func (h *Handler) pgDrop(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		Database string `json:"database"`
		Confirm  string `json:"confirm"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := h.service.Do(ops.Request{Method: ops.ActionPGDrop, Database: strings.TrimSpace(request.Database), Confirm: strings.TrimSpace(request.Confirm), Actor: session.Email}); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dropped": strings.TrimSpace(request.Database), "updated_at": time.Now().UTC()})
}

// pgQuery runs one SQL statement, or a batch of them, against a database and returns the last
// result set. It can write, so it goes through Do and lands in the audit log with the statement.
func (h *Handler) pgQuery(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		Database string `json:"database"`
		SQL      string `json:"sql"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.service.Do(ops.Request{Method: ops.ActionPGQuery, Database: strings.TrimSpace(request.Database), SQL: request.SQL, Actor: session.Email})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "updated_at": time.Now().UTC()})
}

// pgConfig writes the backup policy and database selection into the server-only host override
// and hot-reloads the PostgreSQL service, so the checkboxes apply without a daemon restart.
func (h *Handler) pgConfig(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		Backups config.PostgresBackups `json:"backups"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Rescan re-applies the host postgres block, so the service picks the selection up at once.
	result, err := h.service.SaveConfig(session.Email, "pg-config", "host", func() (apps.ConfigFile, error) {
		file, err := h.store.CreateHostLocal()
		if err != nil {
			return file, err
		}
		contents, err := patchPostgresBackup(file.Contents, request.Backups)
		if err != nil {
			return file, err
		}
		return h.store.Write("host", contents, file.Revision)
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	snapshot, _ := h.service.PGSnapshot(false)
	writeJSON(w, http.StatusOK, map[string]any{"file": result.File, "snapshot": snapshot, "backups": h.service.Backups(), "rotation": h.service.PGBackupConfig(), "updated_at": time.Now().UTC()})
}

// patchPostgresBackup replaces the postgres.backups mapping in a host config, leaving every other
// key untouched. Comments in the file are normalized, since the override is machine-managed.
func patchPostgresBackup(contents string, backups config.PostgresBackups) (string, error) {
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
	if err := node.Encode(backups); err != nil {
		return "", err
	}
	setMapping(postgres, "backups", &node)
	out, err := yaml.Marshal(&root)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
