package pg

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dboss/internal/config"
)

// zipArchive builds the shape a dump has on disk: one SQL entry in a zip.
func zipArchive(t *testing.T, sql string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("dump.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(sql)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	return New(config.Config{Dir: dir, StateDir: filepath.Join(dir, ".dboss")}, nil)
}

func TestImportBackupStoresTheArchiveAsItIs(t *testing.T) {
	service := testService(t)
	archive := zipArchive(t, "SELECT 1;")

	entry, err := service.ImportBackup("app", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Manual || entry.Status != "ok" || entry.Database != "app" {
		t.Fatalf("an upload should be recorded as a manual ok entry, got %+v", entry)
	}
	if entry.Bytes != int64(len(archive)) || entry.SHA256 == "" {
		t.Fatalf("entry should carry the archive size and checksum, got %+v", entry)
	}

	stored, err := os.ReadFile(entry.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, archive) {
		t.Fatal("the uploaded archive should land on disk byte for byte")
	}
	if got := filepath.Dir(entry.LocalPath); filepath.Base(got) != "app" || filepath.Base(filepath.Dir(got)) != backupDirName {
		t.Fatalf("archive should live under pg_backup/app, got %s", entry.LocalPath)
	}

	found, path, err := service.BackupFile(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if path != entry.LocalPath || found.ID != entry.ID {
		t.Fatalf("BackupFile should resolve the uploaded entry, got %+v %q", found, path)
	}
	if _, _, err := service.BackupFile("nope"); err == nil {
		t.Fatal("an unknown id should not resolve")
	}
}

func TestImportBackupRefusesWhatCannotBeRestored(t *testing.T) {
	service := testService(t)
	if _, err := service.ImportBackup("app", strings.NewReader("this is not a zip")); err == nil {
		t.Fatal("a non-archive upload should be refused")
	}
	if _, err := service.ImportBackup("../etc", bytes.NewReader(zipArchive(t, "SELECT 1;"))); err == nil {
		t.Fatal("a database name that escapes the backup directory should be refused")
	}
	if entries := service.Backups(); len(entries) != 0 {
		t.Fatalf("a refused upload should record nothing, got %+v", entries)
	}
}

func TestUniqueDumpKeepsIDsApartWithinOneSecond(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)

	first, firstPath := uniqueDump(dir, at)
	if err := os.WriteFile(firstPath, []byte("one"), 0o640); err != nil {
		t.Fatal(err)
	}
	second, secondPath := uniqueDump(dir, at)
	if second == first || secondPath == firstPath {
		t.Fatalf("a second dump in the same second needs its own name, got %q twice", first)
	}
	if !strings.HasSuffix(second, "_2.zip") {
		t.Fatalf("the suffix should follow the stamp, got %q", second)
	}
}
