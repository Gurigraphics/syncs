

<div align="center">

  # Syncs - A Lightweight and Fast File Synchronizer in Go
  
  <img src="https://github.com/user-attachments/assets/1ab5bb29-f228-43d8-8b71-9d27463a4932" width="200" alt="Syncs Logo" />

  <br><br>
</div>


`Syncs` is a command-line tool for real-time, bidirectional file synchronization between two computers on a local network. It was designed as a minimalist, performant, and controllable alternative to heavier solutions, specifically focused on the needs of a development workflow (hot-reload).

This project was born from the need for a solution that doesn't suffer from the random cache issues of **Samba** nor the synchronization delays of **Syncthing**. The architecture is based on WebSockets for instant notifications, a lightweight file manifest, and a streaming chunk system, ensuring synchronization in the order of milliseconds with low memory footprint.

## Main Features

-   **Real-Time Bidirectional Synchronization:** Changes are detected and propagated to the other side almost instantly.
-   **Memory Efficient (Chunking):** Large files are transferred in small (4MB) chunks. You can sync multi-gigabyte files (videos, ISOs) using very little RAM.
-   **Offline Recovery:** Automatically detects changes made while the program was closed (edits, creations, or deletions) and synchronizes them upon startup.
-   **Permission Preservation:** Preserves file permissions (executable bits), ensuring scripts and binaries remain runnable after sync.
-   **Lightweight:** Uses a JSON manifest instead of a heavy database. Doesn't consume resources when there are no changes.
-   **Simple Client-Server Architecture:** Direct communication via WebSockets, protected by password.
-   **Security Guard:** Automatic IP banning system that blocks addresses after a configurable number of failed authentication attempts (Brute-force protection).
-   **Intelligent Optimization:** File content hash is only calculated to resolve ambiguities.
-   **Robust Conflict Resolution:** Uses a "conflict window" based on network latency. When a conflict is detected, both versions of the file are preserved (`file_conflict_user1.txt`), ensuring zero data loss.
-   **Smart Ignore:** Supports a `.syncignore` file (similar to `.gitignore`) and automatically ignores temporary files (`.part`, `~`) and symbolic links to prevent errors.
-   **Compression:** Automatically compresses data chunks during transfer if they exceed a threshold, reducing bandwidth usage.
-   **Cross-Platform:** Compiles and runs natively on Windows and Linux.

## How It Works

`Syncs` operates with a client-server model.

1.  **Configuration:** All operation is defined by a single `config.json` located next to the executable.
2.  **Initialization & Pruning:** On startup, `Syncs` scans the local disk. It detects files created offline and removes entries from the manifest that were deleted offline.
3.  **Connection:** The client establishes a secure WebSocket connection with the server.
4.  **Handshake & Bidirectional Sync:**
    *   The client requests the server's manifest.
    *   **Pull:** The client compares it with local files and requests downloads for missing/outdated files.
    *   **Push:** The client checks if it has files the server is missing and uploads them automatically.
5.  **Hot Reload:** After initial synchronization, both sides enter "watch mode" (`fsnotify`).
6.  **Streaming Transfer:** When a file is modified, it is read and sent in chunks. The receiver appends these chunks to a temporary `.part` file and atomically renames it upon completion.
7.  **Conflict Handling:** If two sides edit a file simultaneously (within the network latency window), the system renames the local copy to preserve it and accepts the remote version.

## Quick Start Guide

### 1. Prerequisites

-   Go (version 1.18 or higher) installed on both machines.

### 2. Compilation

Clone or copy the source code to a folder on both computers.

```bash
# Navigate to the project folder
cd /path/to/syncs

# Initialize the Go module (only the first time)
go mod init syncs
go mod tidy

# Compile the executable
# On Linux:
go build -ldflags="-s -w" -o syncs .

# On Windows:
go build -ldflags="-s -w" -o syncs.exe .
```

### 3. Configuration

In the **same folder as the executable**, create a `config.json` file.

#### Configuration Options

- **user_identity.username**: Identifier for this sync instance (used in conflict resolution).
- **user_identity.mode**: Either "server" or "client".
- **network.server_port**: Port for the WebSocket server.
- **network.ip**: Server IP address (only needed for client mode).
- **security.connection_password**: Password for WebSocket authentication.
- **security.max_login_attempts**: Maximum number of failed login attempts before banning an IP (default: 3).
- **security.ban_duration_minutes**: Duration in minutes for the IP ban (default: 60).
- **sync_behavior.debounce_milliseconds**: Time to wait before processing file changes (default: 200).
- **sync_behavior.compression_threshold_mb**: Compress files larger than this size (default: 10).
- **shared_folder**: Absolute path to the folder to synchronize.

**Example for the Server (Linux/Mac):**
```json
{
  "user_identity": {
    "username": "server_pc",
    "mode": "server"
  },
  "network": {
    "server_port": 9999,
    "ip": "",
    "allowed_origins": [],
    "retry_attempts": 3,
    "retry_delay_ms": 2000
  },
  "security": {
    "connection_password": "StrongPassword123!",
    "max_login_attempts": 3,
    "ban_duration_minutes": 60
  },
  "sync_behavior": {
    "debounce_milliseconds": 200,
    "conflict_window_margin_ms": 50,
    "ignore_files_without_extension": true,
    "compression_threshold_mb": 10,
    "progress_threshold_mb": 50
  },
  "logging": {
    "enabled": true,
    "level": "info",
    "log_to_file": true
  },
  "shared_folder": "/home/user/shared_folder"
}
```

**Example for the Client (Windows):**
```json
{
  "user_identity": {
    "username": "client_pc",
    "mode": "client"
  },
  "network": {
    "server_port": 9999,
    "ip": "192.168.1.50",  // <-- REPLACE WITH SERVER IP
    "allowed_origins": [],
    "retry_attempts": 3,
    "retry_delay_ms": 2000
  },
  "security": {
    "connection_password": "StrongPassword123!",
    "max_login_attempts": 3,
    "ban_duration_minutes": 60
  },
  "sync_behavior": {
    "debounce_milliseconds": 200,
    "conflict_window_margin_ms": 50,
    "ignore_files_without_extension": true,
    "compression_threshold_mb": 10,
    "progress_threshold_mb": 50
  },
  "logging": {
    "enabled": true,
    "level": "info",
    "log_to_file": true
  },
  "shared_folder": "C:\\Users\\User\\Documents\\SyncFolder"
}
```

### 4. Prepare the Shared Folder

In the folder you defined in `shared_folder`, create a `.syncignore` file.

**Example of `.syncignore`:**
```
# Syncs internal files
.sync_meta/
*.part

# Executables and binaries
*.exe
*.dll
*.so

# Dependencies
node_modules/
vendor/
.venv/

# System
.DS_Store
Thumbs.db
.git/
```

### 5. Execution

Run the program **without arguments**. It will read everything from `config.json`.

**On the Server:**
```bash
./syncs
```

**On the Client:**
```cmd
syncs.exe
```

---

## Troubleshooting

### "Connection failed: dial tcp ..."
If you see this error on the client:
1.  **Check the IP:** Ensure `network.ip` in the client's config matches the server's Local IP (e.g., `192.168.1.x`).
2.  **Firewall:** This is the most common cause.
    *   **Windows:** Open "Windows Defender Firewall with Advanced Security", create an Inbound Rule to allow TCP on port 9999 (or allow the `syncs.exe` app).
    *   **Linux:** Use `sudo ufw allow 9999`.

### "File in use" errors
Syncs tries to respect file locks. If a file is locked by another process (like Word or Excel), Syncs might retry later or log a warning. Ensure files aren't heavily locked during initial sync.

## Project Structure

```
syncs/
├── go.mod
├── go.sum
├── main.go             # Entry point, orchestrator & init logic
├── config/
│   └── config.go       # Configuration loading & validation
├── fileops/
│   └── fileops.go      # Hashing, Gzip compression, Chunk writing
├── manifest/
│   └── manifest.go     # Logic for tracking file state & pruning
├── network/
│   └── websocket.go    # WebSocket Server/Client implementation
├── sync/
│   └── core.go         # Synchronization logic (The Brain)
├── types/
│   └── types.go        # Shared structs (FileChunk, ManifestData)
└── watcher/
    └── watcher.go      # File system monitoring (fsnotify)
```
