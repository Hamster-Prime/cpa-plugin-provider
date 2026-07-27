package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const hostPatchArchivePath = "patches/CLIProxyAPI-v7.2.102-plugin-host-security.patch"

var releaseSupportFiles = []string{
	"README.md",
	"LICENSE",
	"THIRD_PARTY_NOTICES.md",
	"LICENSES/Apache-2.0.txt",
	hostPatchArchivePath,
}

type archiveEntry struct {
	SourcePath  string
	ArchivePath string
	Mode        fs.FileMode
}

func main() {
	libraryPath := flag.String("library", "", "compiled plugin library")
	archivePath := flag.String("archive", "", "output zip archive")
	checksumPath := flag.String("checksum", "", "output checksum file")
	repositoryRoot := flag.String("root", ".", "repository root containing release support files")
	flag.Parse()
	if *libraryPath == "" || *archivePath == "" || *checksumPath == "" {
		fatalf("library, archive, and checksum are required")
	}
	if sameFilePath(*archivePath, *checksumPath) {
		fatalf("archive and checksum paths must be different")
	}

	data, err := packageRelease(*libraryPath, *archivePath, *repositoryRoot)
	if err != nil {
		fatalf("%v", err)
	}
	sum := sha256.Sum256(data)
	line := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), filepath.Base(*archivePath))
	if err = os.WriteFile(*checksumPath, []byte(line), 0o644); err != nil {
		fatalf("write checksum: %v", err)
	}
}

func packageRelease(libraryPath, archivePath, repositoryRoot string) ([]byte, error) {
	entries := []archiveEntry{{
		SourcePath:  libraryPath,
		ArchivePath: filepath.Base(libraryPath),
		Mode:        0o755,
	}}
	for _, name := range releaseSupportFiles {
		entries = append(entries, archiveEntry{
			SourcePath:  filepath.Join(repositoryRoot, filepath.FromSlash(name)),
			ArchivePath: name,
			Mode:        0o644,
		})
	}
	return packageArchive(archivePath, entries)
}

func packageArchive(archivePath string, entries []archiveEntry) ([]byte, error) {
	if err := validateArchiveEntries(archivePath, entries); err != nil {
		return nil, err
	}

	archive, err := os.Create(archivePath)
	if err != nil {
		return nil, fmt.Errorf("create archive: %w", err)
	}
	writer := zip.NewWriter(archive)
	for _, item := range entries {
		if err = writeArchiveEntry(writer, item); err != nil {
			break
		}
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if closeErr := archive.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	return os.ReadFile(archivePath)
}

func validateArchiveEntries(archivePath string, entries []archiveEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("archive must contain at least one entry")
	}
	seen := make(map[string]struct{}, len(entries))
	for _, item := range entries {
		name, err := safeArchivePath(item.ArchivePath)
		if err != nil {
			return err
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate archive path %q", name)
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(item.SourcePath) == "" {
			return fmt.Errorf("archive source for %q is empty", name)
		}
		if sameFilePath(item.SourcePath, archivePath) {
			return fmt.Errorf("archive output cannot also be source %q", item.SourcePath)
		}
		info, err := os.Lstat(item.SourcePath)
		if err != nil {
			return fmt.Errorf("stat archive source %q: %w", item.SourcePath, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive source %q is not a regular file", item.SourcePath)
		}
	}
	return nil
}

func safeArchivePath(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	normalized := strings.ReplaceAll(trimmed, "\\", "/")
	cleaned := path.Clean(normalized)
	firstSegment := strings.SplitN(cleaned, "/", 2)[0]
	if name != trimmed || trimmed != normalized || normalized == "" || cleaned == "." || path.IsAbs(normalized) || strings.Contains(firstSegment, ":") || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != normalized {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	return cleaned, nil
}

func writeArchiveEntry(writer *zip.Writer, item archiveEntry) error {
	source, err := os.Open(item.SourcePath)
	if err != nil {
		return fmt.Errorf("open archive source %q: %w", item.SourcePath, err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat archive source %q: %w", item.SourcePath, err)
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return fmt.Errorf("create zip header for %q: %w", item.ArchivePath, err)
	}
	header.Name = item.ArchivePath
	header.Method = zip.Deflate
	header.SetMode(item.Mode)
	destination, err := writer.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create zip entry %q: %w", item.ArchivePath, err)
	}
	if _, err = io.Copy(destination, source); err != nil {
		return fmt.Errorf("write zip entry %q: %w", item.ArchivePath, err)
	}
	return nil
}

func sameFilePath(left, right string) bool {
	leftPath, errLeft := filepath.Abs(left)
	rightPath, errRight := filepath.Abs(right)
	return errLeft == nil && errRight == nil && filepath.Clean(leftPath) == filepath.Clean(rightPath)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
