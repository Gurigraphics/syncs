// watcher/watcher.go
package watcher

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"syncs/config"
	"syncs/types"

	"github.com/fsnotify/fsnotify"
	"github.com/gobwas/glob"
)

// normalizePath converts Windows backslashes to forward slashes for cross-platform compatibility
func normalizePath(path string) string {
	return strings.ReplaceAll(path, "\\", "/")
}

// Watcher encapsulates the fsnotify logic, debouncing, and filtering.
type Watcher struct {
	cfg            *config.Config
	sharedDir      string
	ignorePatterns []glob.Glob
	notifyChan     chan<- types.FileEvent
	fsWatcher      *fsnotify.Watcher
	debounceTimers map[string]*time.Timer
	debounceMutex  sync.Mutex
	debounceDelay  time.Duration
}

// NewWatcher creates and initializes a new file watcher.
func NewWatcher(sharedDir string, cfg *config.Config, ignorePatterns []glob.Glob, debounceDelay time.Duration, notifyChan chan<- types.FileEvent) (*Watcher, error) {
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		cfg:            cfg,
		sharedDir:      sharedDir,
		ignorePatterns: ignorePatterns,
		notifyChan:     notifyChan,
		fsWatcher:      fsWatcher,
		debounceTimers: make(map[string]*time.Timer),
		debounceDelay:  debounceDelay,
	}
	return w, nil
}

// Start begins the main watching loop.
func (w *Watcher) Start() {
	defer w.fsWatcher.Close()

	// Recursive Add with Ignore Logic
	err := filepath.Walk(w.sharedDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, _ := filepath.Rel(w.sharedDir, path)

		// Don't watch ignored folders or internal metadata
		if info.IsDir() && !IsIgnored(relPath, w.ignorePatterns) && !strings.Contains(path, ".sync_meta") {
			if err := w.fsWatcher.Add(path); err != nil {
				log.Printf("WARNING: Could not watch folder '%s': %v", path, err)
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("ERROR starting recursive watching: %v", err)
	}

	log.Println("File watcher started.")
	for {
		select {
		case event, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}
			w.handleEvent(event)
		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			log.Printf("File watcher error: %v", err)
		}
	}
}

// handleEvent processes a fsnotify event.
func (w *Watcher) handleEvent(event fsnotify.Event) {
	relPath, err := filepath.Rel(w.sharedDir, event.Name)
	if err != nil {
		return
	}

	relPath = normalizePath(relPath)

	// 1. Ignore internal files, temp files, and PARTIAL DOWNLOADS
	if strings.Contains(event.Name, ".sync_meta") ||
		strings.HasSuffix(event.Name, "~") ||
		strings.HasSuffix(event.Name, ".tmp") ||
		strings.HasSuffix(event.Name, ".part") { // <-- IMPORTANT: Ignores the chunking file
		return
	}

	// 2. Explicitly ignore Symlinks to prevent loops/crashes
	info, err := os.Lstat(event.Name)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		// log.Printf("[Watcher] Ignoring Symlink: %s", relPath)
		return
	}

	// 3. Check user-defined ignore patterns (.syncignore)
	if IsIgnored(relPath, w.ignorePatterns) {
		return
	}

	if event.Op&fsnotify.Chmod == fsnotify.Chmod {
		return
	}

	// 4. Check for files without extensions if configured
	if w.cfg.SyncBehavior.IgnoreFilesWithoutExtension {
		// Re-stat to follow symlinks if needed, or check existence
		if statInfo, err := os.Stat(event.Name); err == nil && !statInfo.IsDir() {
			fileName := filepath.Base(event.Name)
			if !strings.Contains(fileName, ".") {
				log.Printf("[Watcher] Ignoring file without extension: %s", relPath)
				return
			}
		}
	}

	// Auto-watch new directories
	if event.Op&fsnotify.Create == fsnotify.Create {
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
			w.fsWatcher.Add(event.Name)
		}
	}

	// Debounce Logic
	w.debounceMutex.Lock()
	defer w.debounceMutex.Unlock()

	if timer, exists := w.debounceTimers[event.Name]; exists {
		timer.Reset(w.debounceDelay)
		return
	}

	w.debounceTimers[event.Name] = time.AfterFunc(w.debounceDelay, func() {
		log.Printf("[Watcher] Confirmed change for: %s (Op: %s)", relPath, event.Op)
		w.notifyChan <- types.FileEvent{Path: relPath, Op: event.Op}

		w.debounceMutex.Lock()
		delete(w.debounceTimers, event.Name)
		w.debounceMutex.Unlock()
	})
}

// LoadIgnorePatterns loads ignore patterns from the .syncignore file.
func LoadIgnorePatterns(sharedDir string) ([]glob.Glob, error) {
	var patterns []glob.Glob
	ignorePath := filepath.Join(sharedDir, ".syncignore")
	f, err := os.Open(ignorePath)
	if os.IsNotExist(err) {
		return patterns, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			g, err := glob.Compile(line)
			if err == nil {
				patterns = append(patterns, g)
			} else {
				log.Printf("WARNING: Invalid pattern in .syncignore: '%s'", line)
			}
		}
	}
	return patterns, scanner.Err()
}

// IsIgnored checks if a path matches any of the ignore patterns.
func IsIgnored(path string, patterns []glob.Glob) bool {
	path = normalizePath(path)
	for _, p := range patterns {
		if p.Match(path) {
			return true
		}
	}
	return false
}
