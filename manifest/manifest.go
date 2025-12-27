// manifest/manifest.go
package manifest

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"syncs/config"
	"syncs/fileops"
	"syncs/types"
	"syncs/watcher"

	"github.com/gobwas/glob"
)

// normalizePath converts Windows backslashes to forward slashes for cross-platform compatibility
func normalizePath(path string) string {
	return strings.ReplaceAll(path, "\\", "/")
}

// Manifest manages the file manifest for synchronization.
type Manifest struct {
	Data        types.ManifestData
	mutex       sync.RWMutex
	SharedDir   string
	ignoreGlobs []glob.Glob
	cfg         *config.Config
}

// NewManifest creates a new Manifest instance.
func NewManifest(sharedDir string, ignorePatterns []glob.Glob, cfg *config.Config) (*Manifest, error) {
	m := &Manifest{
		Data: types.ManifestData{
			Paths:  make(map[string]string),
			Hashes: make(map[string]types.FileMetadata),
		},
		SharedDir:   sharedDir,
		ignoreGlobs: ignorePatterns,
		cfg:         cfg,
	}

	// Try to load the existing manifest
	if err := m.Load(); err != nil {
		log.Printf("Warning: Manifest not found or invalid. Creating a new one.")
	}

	// Always run Scan on initialization to align with disk reality
	if err := m.ScanAndBuild(); err != nil {
		return nil, fmt.Errorf("failed to create initial manifest: %w", err)
	}

	return m, nil
}

func (m *Manifest) manifestPath() string {
	return filepath.Join(m.SharedDir, ".sync_meta", "manifest.json")
}

// Load reads the manifest from disk.
func (m *Manifest) Load() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	f, err := os.Open(m.manifestPath())
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&m.Data); err != nil {
		return fmt.Errorf("the manifest is corrupted: %w", err)
	}
	// Normalize paths to use forward slashes
	for path, hash := range m.Data.Paths {
		normalizedPath := normalizePath(path)
		if normalizedPath != path {
			delete(m.Data.Paths, path)
			m.Data.Paths[normalizedPath] = hash
		}
	}
	return nil
}

// Save writes the manifest data to disk atomically.
func (m *Manifest) Save() error {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	metaDir := filepath.Dir(m.manifestPath())
	if err := os.MkdirAll(metaDir, 0755); err != nil {
		return err
	}
	tempPath := m.manifestPath() + ".tmp"
	f, err := os.Create(tempPath)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(m.Data)
	f.Close()
	if err != nil {
		os.Remove(tempPath)
		return err
	}
	return os.Rename(tempPath, m.manifestPath())
}

// ScanAndBuild scans the disk, updates existing files, and removes deleted files.
func (m *Manifest) ScanAndBuild() error {
	// log.Println("Scanning disk to update manifest...") // Optional verbose logging

	// Track what actually exists on disk
	existsOnDisk := make(map[string]bool)

	err := filepath.Walk(m.SharedDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(m.SharedDir, path)

		// Convert "folder\file" (Win) to "folder/file" (default)
		relPath = normalizePath(relPath)

		// Ignore directories, metadata, and ignored patterns
		if info.IsDir() || strings.Contains(path, ".sync_meta") || watcher.IsIgnored(relPath, m.ignoreGlobs) {
			return nil
		}

		// Ignore files without extensions in scan
		if m.cfg.SyncBehavior.IgnoreFilesWithoutExtension && !info.IsDir() {
			fileName := filepath.Base(path)
			if !strings.Contains(fileName, ".") {
				return nil
			}
		}

		m.UpdateEntry(relPath)
		existsOnDisk[relPath] = true
		return nil
	})

	if err != nil {
		return err
	}

	// Cleanup (Pruning): Remove from manifest what is NOT on disk
	m.mutex.Lock()
	var toDelete []string
	for path := range m.Data.Paths {
		if !existsOnDisk[path] {
			toDelete = append(toDelete, path)
		}
	}

	// Delete effectively outside the range loop to avoid errors
	for _, path := range toDelete {
		// Remove from Paths
		if hash, ok := m.Data.Paths[path]; ok {
			delete(m.Data.Paths, path)
			// Note: We don't delete from the 'Hashes' map immediately because other files may have the same content (hash).
			// Garbage collection of orphan hashes can be done later, but it's not critical now.
			_ = hash
		}
		// log.Printf("Manifest Pruning: Removed ghost entry '%s'", path) // Debug
	}
	m.mutex.Unlock()

	return m.Save()
}

// UpdateEntry updates the manifest entry for a given relative path.
func (m *Manifest) UpdateEntry(relPath string) (*types.FileMetadata, string, bool) {
	// Normalize path to forward slashes (cross-platform compatibility)
	relPath = normalizePath(relPath)

	fullPath := filepath.Join(m.SharedDir, relPath)
	info, err := os.Stat(fullPath)
	if err != nil {
		return nil, "", false
	}

	hash, err := fileops.GetFileHash(fullPath)
	if err != nil {
		return nil, "", false
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	// If the hash is the same, the content hasn't changed
	if oldHash, ok := m.Data.Paths[relPath]; ok && oldHash == hash {
		meta := m.Data.Hashes[hash]
		return &meta, hash, false
	}

	// Version logic
	version := 1
	if oldHash, ok := m.Data.Paths[relPath]; ok {
		if oldMeta, ok := m.Data.Hashes[oldHash]; ok {
			version = oldMeta.Version + 1
		}
	}

	newMeta := types.FileMetadata{
		LastUser:    m.cfg.UserIdentity.Username,
		LastModTime: info.ModTime().UnixNano(),
		Version:     version,
		Mode:        info.Mode(),
	}

	m.Data.Paths[relPath] = hash
	m.Data.Hashes[hash] = newMeta
	return &newMeta, hash, true
}

func (m *Manifest) GetEntry(relPath string) (string, types.FileMetadata, bool) {
	// Normalize path to forward slashes (cross-platform compatibility)
	relPath = normalizePath(relPath)

	m.mutex.RLock()
	defer m.mutex.RUnlock()
	hash, ok := m.Data.Paths[relPath]
	if !ok {
		return "", types.FileMetadata{}, false
	}
	meta, ok := m.Data.Hashes[hash]
	return hash, meta, ok
}

func (m *Manifest) HasEntry(relPath string) bool {
	// Normalize path to forward slashes (cross-platform compatibility)
	relPath = normalizePath(relPath)

	m.mutex.RLock()
	defer m.mutex.RUnlock()
	_, ok := m.Data.Paths[relPath]
	return ok
}

func (m *Manifest) DeleteEntry(relPath string) *types.FileMetadata {
	// Normalize path to forward slashes (cross-platform compatibility)
	relPath = normalizePath(relPath)

	m.mutex.Lock()
	defer m.mutex.Unlock()
	if oldHash, ok := m.Data.Paths[relPath]; ok {
		if oldMeta, ok := m.Data.Hashes[oldHash]; ok {
			delete(m.Data.Paths, relPath)
			// delete(m.Data.Hashes, oldHash) // Opcional
			return &oldMeta
		}
	}
	return nil
}

// Refresh forces an update/prune of the manifest.
func (m *Manifest) Refresh() error {
	return m.ScanAndBuild()
}
