package cli

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestArchiveWriterWritesJSONBytesTextAndFileEntries(t *testing.T) {
	tmpDir := t.TempDir()
	sourcePath := filepath.Join(tmpDir, "application.log")
	if err := os.WriteFile(sourcePath, []byte("from disk\n"), 0644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	archivePath := filepath.Join(tmpDir, "diagnostics.zip")
	archive, err := newArchiveWriter(archivePath)
	if err != nil {
		t.Fatalf("newArchiveWriter returned error: %v", err)
	}
	if err := archive.writeJSON("checks.json", map[string]any{"checks": []any{}}); err != nil {
		t.Fatalf("writeJSON returned error: %v", err)
	}
	if err := archive.writeJSON("manifest.json", map[string]any{"ok": true}); err != nil {
		t.Fatalf("writeJSON returned error: %v", err)
	}
	if err := archive.writeBytes("dynamodb\\job-42.ddb.json", []byte("{}\n")); err != nil {
		t.Fatalf("writeBytes returned error: %v", err)
	}
	if err := archive.writeText("server/job-42.jsonl", `{"message":"server"}`+"\n"); err != nil {
		t.Fatalf("writeText returned error: %v", err)
	}
	if err := archive.writeFile("logs\\application.log", sourcePath); err != nil {
		t.Fatalf("writeFile returned error: %v", err)
	}
	if err := archive.writeText("instances\\i-123\\console.log", "console line\n"); err != nil {
		t.Fatalf("writeText returned error: %v", err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	files := readArchiveWriterTestFiles(t, archivePath)
	gotNames := sortedArchiveWriterTestFileNames(files)
	wantNames := []string{
		"checks.json",
		"dynamodb/job-42.ddb.json",
		"instances/i-123/console.log",
		"logs/application.log",
		"manifest.json",
		"server/job-42.jsonl",
	}
	if strings.Join(gotNames, "\n") != strings.Join(wantNames, "\n") {
		t.Fatalf("expected archive entries %v, got %v", wantNames, gotNames)
	}
	if !strings.Contains(files["manifest.json"], `"ok": true`) {
		t.Fatalf("manifest.json did not contain JSON payload: %s", files["manifest.json"])
	}
	if files["logs/application.log"] != "from disk\n" {
		t.Fatalf("file entry content mismatch: %q", files["logs/application.log"])
	}

	t.Logf("archive entries: %s", strings.Join(gotNames, ", "))
}

func readArchiveWriterTestFiles(t *testing.T, archivePath string) map[string]string {
	t.Helper()

	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer reader.Close()

	files := make(map[string]string, len(reader.File))
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open archive entry %s: %v", file.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read archive entry %s: %v", file.Name, err)
		}
		files[filepath.ToSlash(file.Name)] = string(data)
	}
	return files
}

func sortedArchiveWriterTestFileNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
