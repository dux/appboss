package console

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/authcog"
	"dboss/internal/config"
	"dboss/internal/ops"
	"dboss/internal/pg"
	"dboss/internal/sysinfo"
)

// pgHandler is a console whose PostgreSQL service is real but has no server: uploading and
// downloading an archive never touches one.
func pgHandler(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Apps = "/apps"
	cfg.Dir = dir
	cfg.StateDir = filepath.Join(dir, ".dboss")
	cfg.Management.Host = config.List{"dboss.lvh.me", "dboss.internal"}
	cfg.Management.Admins = []string{"admin@example.com"}
	cfg.Postgres.Enabled = true

	service := ops.New(&fakeManager{}, fakeLogs{}, pg.New(cfg, nil), nil, nil, nil)
	handler, err := New(cfg, authcog.NewWithKey([]byte("01234567890123456789012345678901")), service, newFakeStore(), &fakeSys{snapshot: sysinfo.Snapshot{Host: sysinfo.Host{Hostname: "box"}}})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func dumpArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("BACKUP.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("SELECT 1;\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func uploadBackup(t *testing.T, handler *Handler, database string, archive []byte) *httptest.ResponseRecorder {
	t.Helper()
	cookie, session := sessionCookie(t, handler)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "BACKUP_2026-09-21T04-00-00Z.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(archive); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/api/pg/backup/upload?database="+database, &body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("Origin", "http://dboss.lvh.me:8081")
	request.Header.Set("X-CSRF-Token", session.CSRF)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestPGBackupUploadAndDownloadRoundTrip(t *testing.T) {
	handler := pgHandler(t)
	archive := dumpArchive(t)

	uploaded := uploadBackup(t, handler, "app", archive)
	if uploaded.Code != http.StatusOK {
		t.Fatalf("upload should be accepted: %d %s", uploaded.Code, uploaded.Body.String())
	}
	var result struct {
		Backup pg.Backup `json:"backup"`
	}
	if err := json.Unmarshal(uploaded.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Backup.Database != "app" || !result.Backup.Manual {
		t.Fatalf("upload should record a manual entry for the database: %+v", result.Backup)
	}

	cookie, session := sessionCookie(t, handler)
	download := call(t, handler, cookie, session, http.MethodGet, "/api/pg/backup/download?id="+result.Backup.ID, "")
	if download.Code != http.StatusOK {
		t.Fatalf("download should serve the archive: %d %s", download.Code, download.Body.String())
	}
	if !bytes.Equal(download.Body.Bytes(), archive) {
		t.Fatal("download should return the archive byte for byte")
	}
	if got := download.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, result.Backup.ID) {
		t.Fatalf("download should be an attachment named after the dump, got %q", got)
	}

	missing := call(t, handler, cookie, session, http.MethodGet, "/api/pg/backup/download?id=nope", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("an unknown id should be a 404: %d", missing.Code)
	}
}

func TestPGBackupUploadRefusesANonArchive(t *testing.T) {
	handler := pgHandler(t)
	response := uploadBackup(t, handler, "app", []byte("not a zip"))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a non-archive upload should be refused: %d %s", response.Code, response.Body.String())
	}
}
