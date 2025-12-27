// config/config.go
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"time"
)

// Config defines the structure of the config.json file.
// This struct represents the entire configuration loaded from config.json.
type Config struct {
	UserIdentity struct {
		Username string `json:"username"` // Username for identification in sync operations
		Mode     string `json:"mode"`     // Mode: "server" or "client"
	} `json:"user_identity"`
	Network struct {
		ServerPort     int      `json:"server_port"`     // Port for the server to listen on (server) or connect to (client)
		IP             string   `json:"ip"`              // IP address of the server (only for client mode)
		AllowedOrigins []string `json:"allowed_origins"` // Allowed origins for CORS (if applicable)
		RetryAttempts  int      `json:"retry_attempts"`  // Number of retry attempts for network operations
		RetryDelayMs   int      `json:"retry_delay_ms"`  // Delay between retry attempts in milliseconds
	} `json:"network"`
	Security struct {
		ConnectionPassword string `json:"connection_password"`  // Password for authenticating the WebSocket connection
		MaxLoginAttempts   int    `json:"max_login_attempts"`   //
		BanDurationMinutes int    `json:"ban_duration_minutes"` //
	} `json:"security"`
	SyncBehavior struct {
		DebounceMilliseconds        int  `json:"debounce_milliseconds"`          // Milliseconds to debounce file change events
		ConflictWindowMarginMs      int  `json:"conflict_window_margin_ms"`      // Margin for conflict window based on network latency
		IgnoreFilesWithoutExtension bool `json:"ignore_files_without_extension"` // Whether to ignore files without extensions
		CompressionThresholdMB      int  `json:"compression_threshold_mb"`       // Compress files larger than this size in MB (0 to disable)
		ProgressThresholdMB         int  `json:"progress_threshold_mb"`          // Report progress for files larger than this size in MB (0 to disable)
	} `json:"sync_behavior"`
	Logging struct {
		Enabled   bool   `json:"enabled"`     // Enable logging
		Level     string `json:"level"`       // Log level: "debug", "info", "warn", "error"
		LogToFile bool   `json:"log_to_file"` // Whether to log to file in addition to stdout
	} `json:"logging"`
	SharedFolder string `json:"shared_folder"` // Path to the shared folder to synchronize
}

// Load loads the configuration from the config.json file located in the current working directory or next to the executable.
// It sets default values, decodes the JSON, and validates the configuration.
func Load() (*Config, error) {
	// First, try to find config.json in the current working directory
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("could not get current working directory: %w", err)
	}
	configPath := filepath.Join(cwd, "config.json")

	f, err := os.Open(configPath)
	if err != nil {
		// If not found in cwd, try next to the executable (for built binaries)
		exePath, exeErr := os.Executable()
		if exeErr != nil {
			return nil, fmt.Errorf("could not find the executable path: %w", exeErr)
		}
		configPath = filepath.Join(filepath.Dir(exePath), "config.json")
		f, err = os.Open(configPath)
		if err != nil {
			return nil, fmt.Errorf("configuration file '%s' not found. Create it in the current directory or next to the executable", configPath)
		}
	}
	defer f.Close()

	var cfg Config
	setDefaults(&cfg)

	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to read config.json: %w", err)
	}

	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &cfg, nil
}

// setDefaults sets default values for configuration fields that are not specified in config.json.
func setDefaults(cfg *Config) {
	cfg.SyncBehavior.DebounceMilliseconds = 200
	cfg.SyncBehavior.ConflictWindowMarginMs = 50
	cfg.SyncBehavior.IgnoreFilesWithoutExtension = true
	cfg.SyncBehavior.CompressionThresholdMB = 10 // Compress files larger than 10MB
	cfg.SyncBehavior.ProgressThresholdMB = 50    // Report progress for files larger than 50MB
	cfg.Network.RetryAttempts = 3
	cfg.Network.RetryDelayMs = 2000 // 2 seconds
	cfg.Logging.Enabled = true
	cfg.Logging.Level = "info"
	cfg.Logging.LogToFile = true

	// Basic Security Defaults
	if cfg.Security.MaxLoginAttempts == 0 {
		cfg.Security.MaxLoginAttempts = 3
	}
	if cfg.Security.BanDurationMinutes == 0 {
		cfg.Security.BanDurationMinutes = 60 // 1 hour banned
	}
}

// validateConfig validates the configuration and returns an error if any required fields are missing or invalid.
func validateConfig(cfg *Config) error {
	if cfg.SharedFolder == "" {
		return fmt.Errorf("the 'shared_folder' field cannot be empty")
	}
	if cfg.UserIdentity.Mode == "client" && cfg.Network.IP == "" {
		return fmt.Errorf("in 'client' mode, the 'network.ip' field (server IP) cannot be empty")
	}

	if cfg.UserIdentity.Username == "" {
		return fmt.Errorf("the 'user_identity.username' field cannot be empty")
	}
	if cfg.UserIdentity.Mode != "server" && cfg.UserIdentity.Mode != "client" {
		return fmt.Errorf("the 'user_identity.mode' must be 'server' or 'client'")
	}
	if cfg.Network.ServerPort <= 1024 || cfg.Network.ServerPort > 65535 {
		return fmt.Errorf("the 'network.server_port' must be between 1025 and 65535")
	}
	if len(cfg.Security.ConnectionPassword) < 8 {
		return fmt.Errorf("the 'security.connection_password' must be at least 8 characters long")
	}
	return nil
}

// SetupLogging configures the log destination based on the configuration.
// If logging is enabled and LogToFile is true, logs are written to a file in the shared directory's .sync_meta/logs folder.
// Otherwise, logs are discarded if disabled, or only to stdout.
func SetupLogging(cfg *Config, sharedDir string) {
	if !cfg.Logging.Enabled {
		log.SetOutput(io.Discard)
		return
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	if cfg.Logging.LogToFile {
		logDir := filepath.Join(sharedDir, ".sync_meta", "logs")
		if err := os.MkdirAll(logDir, 0755); err != nil {
			log.Printf("WARNING: Could not create logs folder: %v", err)
			return
		}

		logFileName := fmt.Sprintf("sync_%s.log", time.Now().Format("2006-01-02"))

		logFile, err := os.OpenFile(filepath.Join(logDir, logFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)

		if err != nil {
			log.Printf("WARNING: Could not open log file: %v", err)
			return
		}

		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	}
}
