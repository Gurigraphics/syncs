// types/types.go
package types

import (
	"os"

	"github.com/fsnotify/fsnotify"
)

// FileMetadata holds metadata for a file in the manifest.
type FileMetadata struct {
	LastModTime int64       `json:"last_mod_time"` // Last modification time in nanoseconds
	LastUser    string      `json:"last_user"`     // Username of the last user who modified the file
	Version     int         `json:"version"`       // Version number for conflict resolution
	Mode        os.FileMode `json:"mode"`          // File permissions and mode
}

// WSMessage defines the structure of all WebSocket messages.
type WSMessage struct {
	Type       string      `json:"type"`       // Message type (e.g., "update_notification", "file_chunk")
	Payload    interface{} `json:"payload"`    // Message payload
	Compressed bool        `json:"compressed"` // Whether the payload is compressed
}

// FileEvent represents a confirmed change in the file system.
type FileEvent struct {
	Path string      // Relative path of the file that changed
	Op   fsnotify.Op // Operation type
}

// ManifestData is a pure representation of the manifest data for transfer.
type ManifestData struct {
	Paths  map[string]string       `json:"paths"`  // Map of relative paths to their SHA256 hashes
	Hashes map[string]FileMetadata `json:"hashes"` // Map of hashes to their metadata
}

// FileChunk represents a specific part of a large file being transferred.
type FileChunk struct {
	Path        string      `json:"path"`         // Relative path of the file
	ChunkIndex  int         `json:"chunk_index"`  // Sequence number (0, 1, 2...)
	TotalChunks int         `json:"total_chunks"` // Total number of chunks
	Content     []byte      `json:"content"`      // The actual binary data of this chunk
	Mode        os.FileMode `json:"mode"`         // File permissions (sent mostly for the finalization step)
}
