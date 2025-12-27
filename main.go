// main.go
package main

import (
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"syncs/config"
	"syncs/manifest"
	"syncs/network"
	"syncs/sync"
	"syncs/types"
	"syncs/watcher"
)

// main is the entry point for the Syncs application.
func main() {

	// --- 1. Load Configuration ---
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("FATAL ERROR: %v", err)
	}

	// --- 2. Module Initialization ---
	sharedDir, err := filepath.Abs(cfg.SharedFolder)
	if err != nil {
		log.Fatalf("ERROR: Invalid 'shared_folder' path: %v", err)
	}
	sharedDir = filepath.ToSlash(sharedDir)

	config.SetupLogging(cfg, sharedDir)
	log.Println("--- SESSION START ---")
	log.Printf("Using shared folder: %s", sharedDir)

	if err := os.MkdirAll(filepath.Join(sharedDir, ".sync_meta"), 0755); err != nil {
		log.Fatalf("ERROR: Could not create metadata folder: %v", err)
	}

	ignorePatterns, err := watcher.LoadIgnorePatterns(sharedDir)
	if err != nil {
		log.Printf("WARNING: Could not load .syncignore: %v", err)
	}

	// Initialize Manifest
	manif, err := manifest.NewManifest(sharedDir, ignorePatterns, cfg)
	if err != nil {
		log.Fatalf("Failed to initialize manifest: %v", err)
	}

	// This detects files created/deleted while the program was closed.
	// It effectively "resets" the manifest to reality without deleting the file.
	log.Println("Performing initial file scan...")
	if err := manif.Refresh(); err != nil {
		log.Printf("WARNING: Initial scan failed: %v", err)
	}

	// Create communication channels
	localChanges := make(chan types.FileEvent, 100)
	remoteMessages := make(chan types.WSMessage, 100)
	outgoingMessages := make(chan types.WSMessage, 100)

	// Start the "brain"
	syncCore := sync.NewCore(cfg, manif, localChanges, remoteMessages, outgoingMessages)
	go syncCore.Start()

	// Start the "watcher"
	debounceDelay := time.Duration(cfg.SyncBehavior.DebounceMilliseconds) * time.Millisecond
	fileWatcher, err := watcher.NewWatcher(sharedDir, cfg, ignorePatterns, debounceDelay, localChanges)
	if err != nil {
		log.Fatalf("Failed to create file watcher: %v", err)
	}
	go fileWatcher.Start()

	// Start the "network"
	if cfg.UserIdentity.Mode == "server" {
		go network.StartServer(cfg, remoteMessages, outgoingMessages)
	} else {
		go network.StartClient(cfg, cfg.Network.IP, remoteMessages, outgoingMessages)
	}

	log.Println("Synchronization system started. Press Ctrl+C to exit.")

	// --- 3. Wait for Termination ---
	waitForExitSignal()

	log.Println("Shutting down...")
	manif.Save()
}

func waitForExitSignal() {
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
}
