// network/websocket.go
package network

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"syncs/config"
	"syncs/types"

	"github.com/gorilla/websocket"
)

var (
	upgrader websocket.Upgrader
	ipGuard  *IPGuard
)

type IPGuard struct {
	mu          sync.Mutex
	attempts    map[string]int       // IP -> number of failures
	bans        map[string]time.Time // IP -> ban expiration time
	maxAttempts int
	banDuration time.Duration
}

func NewIPGuard(maxAttempts, banDurationMinutes int) *IPGuard {
	return &IPGuard{
		attempts:    make(map[string]int),
		bans:        make(map[string]time.Time),
		maxAttempts: maxAttempts,
		banDuration: time.Duration(banDurationMinutes) * time.Minute,
	}
}

// IsBanned checks if the IP is banned. Returns true if banned.
func (g *IPGuard) IsBanned(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if expiration, ok := g.bans[ip]; ok {
		if time.Now().Before(expiration) {
			return true // Still banned
		}
		// Ban expired, clear
		delete(g.bans, ip)
		delete(g.attempts, ip)
	}
	return false
}

// RegisterAttempt registers a success or failure attempt.
func (g *IPGuard) RegisterAttempt(ip string, success bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if success {
		// Successful login resets the counter
		delete(g.attempts, ip)
		return
	}

	// Failure
	g.attempts[ip]++
	if g.attempts[ip] >= g.maxAttempts {
		g.bans[ip] = time.Now().Add(g.banDuration)
		log.Printf("[SECURITY] ALERT: IP %s banned for %v after %d failed attempts.", ip, g.banDuration, g.attempts[ip])
	}
}

// StartServer starts the WebSocket server.
// It sets up the HTTP handler and begins listening for connections.
func StartServer(cfg *config.Config, incoming chan<- types.WSMessage, outgoing <-chan types.WSMessage) {
	// Initialize the guard with configs
	ipGuard = NewIPGuard(cfg.Security.MaxLoginAttempts, cfg.Security.BanDurationMinutes)

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return checkOrigin(r, cfg)
		},
		ReadBufferSize:  65536,
		WriteBufferSize: 65536,
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveWs(w, r, cfg, incoming, outgoing)
	})

	addr := fmt.Sprintf(":%d", cfg.Network.ServerPort)
	log.Printf("WebSocket server listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// StartClient starts the WebSocket client.
// It attempts to connect to the server with configurable retry logic, handles reconnection, and manages message pumps.
func StartClient(cfg *config.Config, serverIP string, incoming chan<- types.WSMessage, outgoing <-chan types.WSMessage) {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)

	url := fmt.Sprintf("ws://%s:%d/?password=%s", serverIP, cfg.Network.ServerPort, cfg.Security.ConnectionPassword)
	retryDelay := time.Duration(cfg.Network.RetryDelayMs) * time.Millisecond

	for {
		log.Printf("Attempting to connect to %s...", url)
		dialer := websocket.DefaultDialer
		dialer.ReadBufferSize = 65536
		dialer.WriteBufferSize = 65536

		conn, _, err := dialer.Dial(url, nil)
		if err != nil {
			log.Printf("Connection failed: %v. Retrying in %v...", err, retryDelay)
			select {
			case <-time.After(retryDelay):
				continue
			case <-interrupt:
				return
			}
		}

		log.Println("Successfully connected to server.")

		done := make(chan struct{})

		// Pass config to writePump (to control retries if necessary)
		go writePump(conn, outgoing, done, cfg)
		go readPump(conn, incoming, done)

		// Send initial manifest request
		initMsg := types.WSMessage{Type: "request_full_manifest"}
		if err := conn.WriteJSON(initMsg); err != nil {
			log.Printf("Error requesting initial manifest: %v", err)
			conn.Close() // This will force readPump to close 'done'
			// Do not close done manually here!
			<-done // Wait for readPump to finish
			continue
		}

		select {
		case <-done:
			log.Println("Connection lost. Reconnecting...")
			// No need to close anything here, readPump already closed done.

		case <-interrupt:
			log.Println("Shutting down...")

			// Send graceful close message (best effort)
			err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				log.Printf("Error sending close message: %v", err)
			}

			conn.Close()

			select {
			case <-done:
				// Wait for readPump to clean up
			case <-time.After(time.Second):
				// Safety timeout
			}
			return
		}
	}
}

// serveWs handles WebSocket upgrade and starts message pumps for a client connection.
func serveWs(w http.ResponseWriter, r *http.Request, cfg *config.Config, incoming chan<- types.WSMessage, outgoing <-chan types.WSMessage) {
	// Extract only the IP (without the port)
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr // Fallback if unable to separate
	}

	if ipGuard.IsBanned(ip) {
		log.Printf("[SECURITY] Connection rejected from banned IP: %s", ip)
		http.Error(w, "Forbidden: IP Banned due to too many failed attempts.", http.StatusForbidden)
		return
	}

	if !checkPassword(r, cfg) {
		log.Printf("[SECURITY] Authentication failure from %s", ip)
		ipGuard.RegisterAttempt(ip, false) // Register failure
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ipGuard.RegisterAttempt(ip, true) // Clear previous attempts

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Error upgrading connection: %v", err)
		return
	}
	defer conn.Close()

	log.Printf("Client (%s) connected successfully.", r.RemoteAddr)

	done := make(chan struct{})
	go writePump(conn, outgoing, done, cfg)
	readPump(conn, incoming, done)

	log.Printf("Client (%s) disconnected.", r.RemoteAddr)
}

// readPump reads messages from the WebSocket connection and sends them to the incoming channel.
// It runs in a goroutine and closes the done channel when the connection is closed.
func readPump(conn *websocket.Conn, incoming chan<- types.WSMessage, done chan struct{}) {
	defer close(done)
	for {
		var msg types.WSMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Connection error: %v", err)
			}
			break
		}
		incoming <- msg
	}
}

// writePump writes messages to the WebSocket connection from the outgoing channel.
// It also sends periodic ping messages to keep the connection alive.
// Runs in a goroutine and handles connection closure with retry logic.
func writePump(conn *websocket.Conn, outgoing <-chan types.WSMessage, done <-chan struct{}, cfg *config.Config) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	defer conn.Close()

	log.Println("[Network] Write loop started.")

	for {
		select {
		case msg, ok := <-outgoing:
			if !ok {
				conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			log.Printf("[Network] Sending message type '%s'...", msg.Type)

			// Retry logic for sending messages
			for attempt := 0; attempt <= cfg.Network.RetryAttempts; attempt++ {
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err := conn.WriteJSON(msg)
				if err == nil {
					break // Success
				}
				if attempt < cfg.Network.RetryAttempts {
					log.Printf("[Network] ERROR writing to connection (attempt %d/%d): %v. Retrying...", attempt+1, cfg.Network.RetryAttempts+1, err)
					time.Sleep(time.Duration(cfg.Network.RetryDelayMs) * time.Millisecond)
				} else {
					log.Printf("[Network] ERROR writing to connection after %d attempts: %v", cfg.Network.RetryAttempts+1, err)
					return
				}
			}
		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-done:
			log.Println("[Network] Write loop terminated (done signal received).")
			return
		}
	}
}

// checkPassword verifies the password from the request query parameters.
// Returns true if the password matches the configured connection password.
func checkPassword(r *http.Request, cfg *config.Config) bool {
	password := r.URL.Query().Get("password")
	return password == cfg.Security.ConnectionPassword
}

// checkOrigin verifies if the request origin is allowed based on configuration.
// If no origins are configured, all are allowed. Otherwise, only configured origins are permitted.
func checkOrigin(r *http.Request, cfg *config.Config) bool {
	if len(cfg.Network.AllowedOrigins) == 0 {
		return true
	}
	origin := r.Header.Get("Origin")
	for _, allowed := range cfg.Network.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	log.Printf("Origin '%s' from %s rejected.", origin, r.RemoteAddr)
	return false
}
