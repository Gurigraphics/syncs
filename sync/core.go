// sync/core.go
package sync

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syncs/config"
	"syncs/fileops"
	"syncs/manifest"
	"syncs/types"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ChunkSize defines the size of each file part transmitted (4MB).
// This keeps memory usage low even for multi-gigabyte files.
const ChunkSize = 4 * 1024 * 1024

var recentlyWritten sync.Map

type Core struct {
	cfg              *config.Config
	manifest         *manifest.Manifest
	localChanges     <-chan types.FileEvent
	remoteMessages   <-chan types.WSMessage
	outgoingMessages chan<- types.WSMessage
	conflictWindow   time.Duration
}

func NewCore(cfg *config.Config, man *manifest.Manifest, localChanges <-chan types.FileEvent, remoteMessages <-chan types.WSMessage, outgoingMessages chan<- types.WSMessage) *Core {
	return &Core{
		cfg:              cfg,
		manifest:         man,
		localChanges:     localChanges,
		remoteMessages:   remoteMessages,
		outgoingMessages: outgoingMessages,
		conflictWindow:   time.Duration(cfg.SyncBehavior.ConflictWindowMarginMs) * time.Millisecond,
	}
}

func (c *Core) Start() {
	log.Println("Synchronization brain started. Waiting for events...")
	for {
		select {
		case event := <-c.localChanges:
			c.handleLocalChange(event)
		case msg := <-c.remoteMessages:
			c.handleRemoteMessage(msg)
		}
	}
}

func (c *Core) handleLocalChange(event types.FileEvent) {
	if _, exists := recentlyWritten.Load(event.Path); exists {
		return
	}

	if event.Op&fsnotify.Remove == fsnotify.Remove || event.Op&fsnotify.Rename == fsnotify.Rename {
		if c.manifest.HasEntry(event.Path) {
			c.manifest.DeleteEntry(event.Path)
			log.Printf("[CORE] Local deletion of '%s'. Sending notification.", event.Path)
			c.outgoingMessages <- types.WSMessage{
				Type:    "delete_notification",
				Payload: map[string]string{"path": event.Path},
			}
			c.manifest.Save()
		}
		return
	}

	newMeta, hash, changed := c.manifest.UpdateEntry(event.Path)
	if !changed {
		return
	}

	log.Printf("[CORE] Local change to '%s'. Sending update notification.", event.Path)
	c.outgoingMessages <- types.WSMessage{
		Type: "update_notification",
		Payload: map[string]interface{}{
			"path":     event.Path,
			"hash":     hash,
			"metadata": newMeta,
		},
	}
	c.manifest.Save()
}

func (c *Core) handleRemoteMessage(msg types.WSMessage) {
	switch msg.Type {

	case "request_full_manifest":
		log.Println("[CORE] Full manifest request received. Sending data...")
		c.manifest.Refresh()
		c.outgoingMessages <- types.WSMessage{
			Type:    "full_manifest",
			Payload: c.manifest.Data,
		}

	case "full_manifest":
		log.Println("[CORE] Remote manifest received. Starting convergence...")
		var remoteManifest types.ManifestData
		if err := remap(msg.Payload, &remoteManifest); err != nil {
			log.Printf("[CORE] Error parsing remote manifest: %v", err)
			return
		}

		// 1. Download missing files
		var requestList []string
		for path, remoteHash := range remoteManifest.Paths {

			// Do not request files without an extension
			if c.cfg.SyncBehavior.IgnoreFilesWithoutExtension {
				if !strings.Contains(filepath.Base(path), ".") {
					continue
				}
			}

			localHash, _, exists := c.manifest.GetEntry(path)
			if !exists || localHash != remoteHash {
				requestList = append(requestList, path)
			}
		}
		if len(requestList) > 0 {
			log.Printf("[CORE] Requesting %d files from remote...", len(requestList))
			c.outgoingMessages <- types.WSMessage{Type: "request_files", Payload: requestList}
		}

		// 2. Push local files that remote is missing
		// Note: Depending on implementation, iterating manifest.Data.Paths directly might require locking.
		// Assuming single-threaded Core execution or internal locking:
		for localPath, localHash := range c.manifest.Data.Paths {
			if _, ok := remoteManifest.Paths[localPath]; !ok {
				log.Printf("[CORE] Pushing local file missing on remote: %s", localPath)
				_, meta, _ := c.manifest.GetEntry(localPath)
				c.outgoingMessages <- types.WSMessage{
					Type: "update_notification",
					Payload: map[string]interface{}{
						"path": localPath, "hash": localHash, "metadata": meta,
					},
				}
			}
		}

	case "update_notification":
		var data struct {
			Path     string             `json:"path"`
			Hash     string             `json:"hash"`
			Metadata types.FileMetadata `json:"metadata"`
		}
		remap(msg.Payload, &data)

		// Ignore notifications for files without an extension
		if c.cfg.SyncBehavior.IgnoreFilesWithoutExtension {
			if !strings.Contains(filepath.Base(data.Path), ".") {
				return
			}
		}

		fullPath := filepath.Join(c.manifest.SharedDir, data.Path)
		currentLocalHash, err := fileops.GetFileHash(fullPath)

		if err == nil && currentLocalHash == data.Hash {
			return // Already synced
		}

		_, localMeta, exists := c.manifest.GetEntry(data.Path)
		isConflict := false
		if exists {
			diff := time.Duration(data.Metadata.LastModTime - localMeta.LastModTime)
			if diff < 0 {
				diff = -diff
			}
			if diff <= c.conflictWindow {
				isConflict = true
			}
		}

		if isConflict {
			log.Printf("[CORE] Conflict detected for '%s'. Resolving...", data.Path)
			c.resolveConflict(data.Path)
		} else {
			log.Printf("[CORE] Update detected for '%s'. Requesting file...", data.Path)
			c.outgoingMessages <- types.WSMessage{Type: "request_files", Payload: []string{data.Path}}
		}

	case "delete_notification":
		var data struct {
			Path string `json:"path"`
		}
		remap(msg.Payload, &data)
		if c.manifest.HasEntry(data.Path) {
			log.Printf("[CORE] Deleting '%s'", data.Path)
			fullPath := filepath.Join(c.manifest.SharedDir, data.Path)
			recentlyWritten.Store(data.Path, true)
			time.AfterFunc(2*time.Second, func() { recentlyWritten.Delete(data.Path) })
			os.RemoveAll(fullPath)
			c.manifest.DeleteEntry(data.Path)
			c.manifest.Save()
		}

	// --- UPDATED: Sender Logic using Chunking ---
	case "request_files":
		var requestList []string
		remap(msg.Payload, &requestList)

		for _, path := range requestList {
			fullPath := filepath.Join(c.manifest.SharedDir, path)
			info, err := os.Stat(fullPath)
			if err != nil {
				log.Printf("[CORE] Error stating file '%s': %v", path, err)
				continue
			}

			file, err := os.Open(fullPath)
			if err != nil {
				log.Printf("[CORE] Error opening file '%s': %v", path, err)
				continue
			}

			fileSize := info.Size()
			totalChunks := int(fileSize / int64(ChunkSize))
			if fileSize%int64(ChunkSize) != 0 {
				totalChunks++
			}
			if totalChunks == 0 {
				totalChunks = 1
			} // Handle empty file

			log.Printf("[CORE] Sending '%s' in %d chunks...", path, totalChunks)

			buffer := make([]byte, ChunkSize)
			for i := 0; i < totalChunks; i++ {
				bytesRead, err := file.Read(buffer)
				if err != nil && err.Error() != "EOF" {
					log.Printf("[CORE] Error reading chunk %d: %v", i, err)
					break
				}

				chunkData := buffer[:bytesRead]
				compressed := false
				// Compress if threshold is met and chunk is substantial
				if c.cfg.SyncBehavior.CompressionThresholdMB > 0 && len(chunkData) > 1024 {
					chunkData = fileops.CompressData(chunkData)
					compressed = true
				}

				c.outgoingMessages <- types.WSMessage{
					Type: "file_chunk",
					Payload: types.FileChunk{
						Path:        path,
						ChunkIndex:  i,
						TotalChunks: totalChunks,
						Content:     chunkData,
						Mode:        info.Mode(),
					},
					Compressed: compressed,
				}

				// Yield CPU/Network slightly to prevent socket buffer overflow on localhost
				if i%5 == 0 {
					time.Sleep(2 * time.Millisecond)
				}
			}
			file.Close()
		}

	// --- UPDATED: Receiver Logic using Chunking ---
	case "file_chunk":
		var chunk types.FileChunk
		remap(msg.Payload, &chunk)

		if msg.Compressed {
			chunk.Content = fileops.DecompressData(chunk.Content)
		}

		finalPath := filepath.Join(c.manifest.SharedDir, chunk.Path)
		tempPath := finalPath + ".part"

		// 1. Write/Append Chunk
		isFirst := chunk.ChunkIndex == 0
		err := fileops.WriteChunk(tempPath, chunk.Content, isFirst, chunk.Mode)
		if err != nil {
			log.Printf("[CORE] Error writing chunk %d for '%s': %v", chunk.ChunkIndex, chunk.Path, err)
			return
		}

		// Log periodic progress
		if chunk.ChunkIndex > 0 && chunk.ChunkIndex%50 == 0 {
			log.Printf("[CORE] Progress '%s': %d/%d chunks", chunk.Path, chunk.ChunkIndex+1, chunk.TotalChunks)
		}

		// 2. Finalize if Last Chunk
		if chunk.ChunkIndex == chunk.TotalChunks-1 {
			log.Printf("[CORE] Finalizing transfer of '%s'", chunk.Path)

			recentlyWritten.Store(chunk.Path, true)
			time.AfterFunc(2*time.Second, func() { recentlyWritten.Delete(chunk.Path) })

			if err := os.Rename(tempPath, finalPath); err != nil {
				log.Printf("[CORE] Error renaming finalized file '%s': %v", chunk.Path, err)
				recentlyWritten.Delete(chunk.Path)
				return
			}

			// Restore permissions
			os.Chmod(finalPath, chunk.Mode)

			c.manifest.UpdateEntry(chunk.Path)
			c.manifest.Save()
		}
	}
}

func (c *Core) resolveConflict(path string) {
	fullPath := filepath.Join(c.manifest.SharedDir, path)
	conflictIdentifier := c.cfg.UserIdentity.Username

	newPath, err := fileops.RenameForConflict(fullPath, conflictIdentifier)
	if err != nil {
		log.Printf("[CORE] Error renaming conflict '%s': %v", path, err)
		return
	}
	newRelPath, _ := filepath.Rel(c.manifest.SharedDir, newPath)
	log.Printf("[CORE] Local conflict preserved as '%s'", newRelPath)

	c.manifest.DeleteEntry(path)
	c.manifest.UpdateEntry(newRelPath)

	c.outgoingMessages <- types.WSMessage{Type: "request_files", Payload: []string{path}}
	c.manifest.Save()
}

func remap(source interface{}, dest interface{}) error {
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dest)
}
