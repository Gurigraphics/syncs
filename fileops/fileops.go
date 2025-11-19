// fileops/fileops.go
package fileops

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// GetFileHash calculates the SHA256 hash of a file's content.
func GetFileHash(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", os.ErrNotExist
		}
		return "", fmt.Errorf("failed to open file for hashing '%s': %w", filePath, err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("failed to read file for hashing '%s': %w", filePath, err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ReadFile reads the complete content of a file.
// NOTE: Used only for small operations now. Large transfers use streaming in core.go.
func ReadFile(filePath string) ([]byte, error) {
	return os.ReadFile(filePath)
}

// WriteFile writes data to a file atomically.
// NOTE: Used for small legacy writes or manifest saves.
func WriteFile(filePath string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}
	if perm == 0 {
		perm = 0644
	}

	tempFile, err := os.CreateTemp(filepath.Dir(filePath), filepath.Base(filePath)+".tmp")
	if err != nil {
		return err
	}

	if err := tempFile.Chmod(perm); err != nil {
		log.Printf("Warning: Failed to apply chmod: %v", err)
	}

	_, err = tempFile.Write(data)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return fmt.Errorf("failed to write to temporary file: %w", err)
	}
	tempFile.Close()

	if err := os.Rename(tempFile.Name(), filePath); err != nil {
		os.Remove(tempFile.Name())
		return fmt.Errorf("failed to rename temporary file: %w", err)
	}
	return nil
}

// WriteChunk writes a file chunk to a temporary location.
// If isFirst is true, it truncates/creates the file. Otherwise, it appends.
func WriteChunk(tempPath string, data []byte, isFirst bool, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(tempPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	flags := os.O_APPEND | os.O_WRONLY | os.O_CREATE
	if isFirst {
		flags = os.O_TRUNC | os.O_WRONLY | os.O_CREATE
	}

	// We use 0644 for the temp file, the final permission is applied on rename/finalize
	f, err := os.OpenFile(tempPath, flags, 0644)
	if err != nil {
		return fmt.Errorf("failed to open temp chunk file: %w", err)
	}
	defer f.Close()

	_, err = f.Write(data)
	return err
}

// RenameForConflict renames a local file when a conflict is detected.
func RenameForConflict(filePath, conflictIdentifier string) (string, error) {
	dir := filepath.Dir(filePath)
	ext := filepath.Ext(filePath)
	base := strings.TrimSuffix(filepath.Base(filePath), ext)
	newPath := filepath.Join(dir, fmt.Sprintf("%s_conflict_%s%s", base, conflictIdentifier, ext))

	for i := 1; ; i++ {
		if _, err := os.Stat(newPath); os.IsNotExist(err) {
			break
		}
		newPath = filepath.Join(dir, fmt.Sprintf("%s_conflict_%s(%d)%s", base, conflictIdentifier, i, ext))
	}

	err := os.Rename(filePath, newPath)
	if err != nil {
		return "", err
	}
	return newPath, nil
}

// CompressData compresses the given data using gzip.
func CompressData(data []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		log.Printf("Warning: Failed to compress data: %v", err)
		return data
	}
	if err := gz.Close(); err != nil {
		log.Printf("Warning: Failed to close gzip writer: %v", err)
		return data
	}
	return buf.Bytes()
}

// DecompressData decompresses the given gzip-compressed data.
func DecompressData(data []byte) []byte {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		log.Printf("Warning: Failed to create gzip reader: %v", err)
		return data
	}
	defer reader.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, reader); err != nil {
		log.Printf("Warning: Failed to decompress data: %v", err)
		return data
	}
	return buf.Bytes()
}
