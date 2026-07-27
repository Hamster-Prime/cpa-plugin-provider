package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageReleaseIncludesRequiredFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "README.md"), "readme")
	writeTestFile(t, filepath.Join(root, "LICENSE"), "license")
	writeTestFile(t, filepath.Join(root, "THIRD_PARTY_NOTICES.md"), "notices")
	writeTestFile(t, filepath.Join(root, "LICENSES", "Apache-2.0.txt"), "apache license")
	writeTestFile(t, filepath.Join(root, filepath.FromSlash(hostPatchArchivePath)), "patch")
	libraryPath := filepath.Join(root, "dist", "multi-protocol-provider.so")
	writeTestFile(t, libraryPath, "library")
	archivePath := filepath.Join(root, "release.zip")

	data, err := packageRelease(libraryPath, archivePath, root)
	if err != nil {
		t.Fatalf("packageRelease() error = %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}
	want := map[string]struct {
		content string
		mode    os.FileMode
	}{
		"multi-protocol-provider.so": {content: "library", mode: 0o755},
		"README.md":                  {content: "readme", mode: 0o644},
		"LICENSE":                    {content: "license", mode: 0o644},
		"THIRD_PARTY_NOTICES.md":     {content: "notices", mode: 0o644},
		"LICENSES/Apache-2.0.txt":     {content: "apache license", mode: 0o644},
		hostPatchArchivePath:         {content: "patch", mode: 0o644},
	}
	if len(reader.File) != len(want) {
		t.Fatalf("archive entries = %d, want %d", len(reader.File), len(want))
	}
	for _, file := range reader.File {
		expected, ok := want[file.Name]
		if !ok {
			t.Fatalf("unexpected archive entry %q", file.Name)
		}
		if file.Mode().Perm() != expected.mode {
			t.Errorf("%s mode = %o, want %o", file.Name, file.Mode().Perm(), expected.mode)
		}
		entry, errOpen := file.Open()
		if errOpen != nil {
			t.Fatalf("open %s: %v", file.Name, errOpen)
		}
		var content bytes.Buffer
		if _, errRead := content.ReadFrom(entry); errRead != nil {
			_ = entry.Close()
			t.Fatalf("read %s: %v", file.Name, errRead)
		}
		if errClose := entry.Close(); errClose != nil {
			t.Fatalf("close %s: %v", file.Name, errClose)
		}
		if content.String() != expected.content {
			t.Errorf("%s content = %q, want %q", file.Name, content.String(), expected.content)
		}
	}
}

func TestValidateArchiveEntriesRejectsUnsafeOrDuplicatePaths(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	writeTestFile(t, source, "data")
	tests := []struct {
		name    string
		entries []archiveEntry
		want    string
	}{
		{name: "parent traversal", entries: []archiveEntry{{SourcePath: source, ArchivePath: "../source"}}, want: "unsafe archive path"},
		{name: "absolute", entries: []archiveEntry{{SourcePath: source, ArchivePath: "/source"}}, want: "unsafe archive path"},
		{name: "windows absolute", entries: []archiveEntry{{SourcePath: source, ArchivePath: "C:/source"}}, want: "unsafe archive path"},
		{name: "unclean", entries: []archiveEntry{{SourcePath: source, ArchivePath: "docs/../source"}}, want: "unsafe archive path"},
		{name: "backslash", entries: []archiveEntry{{SourcePath: source, ArchivePath: `docs\source`}}, want: "unsafe archive path"},
		{name: "case insensitive duplicate", entries: []archiveEntry{{SourcePath: source, ArchivePath: "README.md"}, {SourcePath: source, ArchivePath: "readme.md"}}, want: "duplicate archive path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateArchiveEntries(filepath.Join(t.TempDir(), "release.zip"), test.entries)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateArchiveEntries() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateArchiveEntriesRejectsOutputAsInput(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "release.zip")
	writeTestFile(t, archivePath, "existing")
	err := validateArchiveEntries(archivePath, []archiveEntry{{SourcePath: archivePath, ArchivePath: "release.zip"}})
	if err == nil || !strings.Contains(err.Error(), "cannot also be source") {
		t.Fatalf("validateArchiveEntries() error = %v, want output/source collision", err)
	}
}

func writeTestFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(name), err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
