package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// URLData represents the stored URL mapping
type URLData struct {
	Code      string    `json:"code"`
	LongURL   string    `json:"long_url"`
	CreatedAt time.Time `json:"created_at"`
}

// Failures holds the software-simulated failures for demo purposes
type Failures struct {
	Redis     bool `json:"redis"`
	ReplicaDB bool `json:"replica_db"`
	PrimaryDB bool `json:"primary_db"`
}

// Config holds region configurations
type Config struct {
	Region      string
	Port        string
	RedisAddr   string
	ReplicaDSN  string
	PrimaryDSN  string
	RouterURL   string // Used in Mock Mode to talk to the Router's mock DB coordinator
	IsPrimary   bool   // True if this region houses the primary DB
	MockMode    bool   // True if running in zero-dependency in-memory mock mode
}

// Local mock maps for this specific regional instance
type MockDataStore struct {
	mu        sync.RWMutex
	replicaDB map[string]string
	cache     map[string]string
}

// AppServer encapsulates databases, cache, configurations, and simulated failures
type AppServer struct {
	config     Config
	redis      *redis.Client
	replicaDB  *sql.DB
	primaryDB  *sql.DB
	mockStore  MockDataStore
	mu         sync.RWMutex
	failures   Failures
	ctx        context.Context
	
	// Stats for dashboard
	stats struct {
		sync.Mutex
		CacheHits       int `json:"cache_hits"`
		ReplicaHits     int `json:"replica_hits"`
		PrimaryFallback int `json:"primary_fallback"`
		TotalRequests   int `json:"total_requests"`
	}
}

func main() {
	// Parse configurations from environment variables
	region := getEnv("REGION", "us-east")
	port := getEnv("PORT", "8081")
	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
	replicaDSN := getEnv("REPLICA_DB_DSN", "")
	primaryDSN := getEnv("PRIMARY_DB_DSN", "")
	routerURL := getEnv("ROUTER_URL", "http://localhost:8080")
	isPrimary := getEnv("IS_PRIMARY", "false") == "true"
	mockMode := getEnv("MOCK_MODE", "false") == "true"

	config := Config{
		Region:     region,
		Port:       port,
		RedisAddr:  redisAddr,
		ReplicaDSN: replicaDSN,
		PrimaryDSN: primaryDSN,
		RouterURL:  routerURL,
		IsPrimary:  isPrimary,
		MockMode:   mockMode,
	}

	log.Printf("[%s] Starting regional app server (MockMode=%v) on port %s...", region, mockMode, port)

	server := NewAppServer(config)
	if !mockMode {
		server.InitializeConnections()
	}

	// Register routes
	mux := http.NewServeMux()
	mux.HandleFunc("/urls", server.handleCreateURL)
	mux.HandleFunc("/health", server.handleHealth)
	mux.HandleFunc("/stats", server.handleStats)
	mux.HandleFunc("/simulate/fail", server.handleSimulateFail)
	mux.HandleFunc("/simulate/reset", server.handleSimulateReset)
	
	// Mock mode control routes
	mux.HandleFunc("/api/mock/replica/sync", server.handleMockReplicaSync)
	mux.HandleFunc("/api/mock/cache/sync", server.handleMockCacheSync)
	
	mux.HandleFunc("/", server.handleResolveURL)

	log.Fatalf("Server failed: %v", http.ListenAndServe(":"+port, mux))
}

func NewAppServer(config Config) *AppServer {
	s := &AppServer{
		config: config,
		ctx:    context.Background(),
	}
	s.mockStore.replicaDB = make(map[string]string)
	s.mockStore.cache = make(map[string]string)
	return s
}

func (s *AppServer) InitializeConnections() {
	// 1. Initialize Redis Cache
	s.redis = redis.NewClient(&redis.Options{
		Addr:         s.config.RedisAddr,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  1 * time.Second,
		WriteTimeout: 1 * time.Second,
	})

	// 2. Initialize DB Connections with Retry
	if s.config.ReplicaDSN != "" {
		s.replicaDB = connectWithRetry("Replica DB", s.config.ReplicaDSN)
	}
	if s.config.PrimaryDSN != "" {
		s.primaryDB = connectWithRetry("Primary DB", s.config.PrimaryDSN)
	}
}

func connectWithRetry(name, dsn string) *sql.DB {
	var db *sql.DB
	var err error
	for i := 0; i < 5; i++ {
		db, err = sql.Open("postgres", dsn)
		if err == nil {
			err = db.Ping()
			if err == nil {
				log.Printf("Successfully connected to %s", name)
				return db
			}
		}
		log.Printf("Failed to connect to %s (attempt %d/5): %v. Retrying in 3s...", name, i+1, err)
		time.Sleep(3 * time.Second)
	}
	log.Printf("Warning: Could not connect to %s after 5 attempts. Starting in degraded state.", name)
	return db
}

// --- HTTP Handlers ---

// handleCreateURL handles writes (POST /urls)
func (s *AppServer) handleCreateURL(w http.ResponseWriter, r *http.Request) {
	s.incrementRequests()
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	primaryFailed := s.failures.PrimaryDB
	s.mu.RUnlock()

	if primaryFailed {
		s.respondWithError(w, http.StatusServiceUnavailable, "Primary DB is currently down (simulated). Writes unavailable.")
		return
	}

	var req struct {
		URL  string `json:"url"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if req.URL == "" {
		s.respondWithError(w, http.StatusBadRequest, "URL parameter is required")
		return
	}

	if req.Code == "" {
		req.Code = generateShortCode(6)
	}

	now := time.Now()

	// --- Mock Mode Write Handling ---
	if s.config.MockMode {
		// Send write to the router coordinator which manages primary mock DB
		writeURL := fmt.Sprintf("%s/api/mock/write", s.config.RouterURL)
		payload, _ := json.Marshal(map[string]string{
			"code": req.Code,
			"url":  req.URL,
		})
		
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Post(writeURL, "application/json", bytes.NewBuffer(payload))
		if err != nil {
			s.respondWithError(w, http.StatusServiceUnavailable, fmt.Sprintf("Failed to communicate with Router mock coordinator: %v", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusConflict {
			s.respondWithError(w, http.StatusConflict, "Short code already exists")
			return
		}
		if resp.StatusCode != http.StatusCreated {
			s.respondWithError(w, resp.StatusCode, "Mock coordinator error")
			return
		}

		// Success response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(URLData{
			Code:      req.Code,
			LongURL:   req.URL,
			CreatedAt: now,
		})
		return
	}

	// --- Real Mode Write Handling ---
	db := s.primaryDB
	if db == nil && s.config.IsPrimary {
		db = s.replicaDB
	}

	if db == nil {
		s.respondWithError(w, http.StatusServiceUnavailable, "Primary DB connection not configured/available")
		return
	}

	query := `INSERT INTO urls (code, long_url, created_at) VALUES ($1, $2, $3) ON CONFLICT (code) DO NOTHING`
	res, err := db.ExecContext(s.ctx, query, req.Code, req.URL, now)
	if err != nil {
		s.respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to write to Primary DB: %v", err))
		return
	}

	rows, _ := res.RowsAffected()
	if rows == 0 {
		s.respondWithError(w, http.StatusConflict, "Short code already exists")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(URLData{
		Code:      req.Code,
		LongURL:   req.URL,
		CreatedAt: now,
	})
}

// handleResolveURL handles reads (GET /:code)
func (s *AppServer) handleResolveURL(w http.ResponseWriter, r *http.Request) {
	s.incrementRequests()
	code := r.URL.Path[1:]
	if code == "" || code == "favicon.ico" {
		http.NotFound(w, r)
		return
	}

	s.mu.RLock()
	failures := s.failures
	s.mu.RUnlock()

	var longURL string
	var source string
	found := false

	// --- Mock Mode Resolution ---
	if s.config.MockMode {
		// 1. Try Local Mock Cache
		if !failures.Redis {
			s.mockStore.mu.RLock()
			urlVal, ok := s.mockStore.cache[code]
			s.mockStore.mu.RUnlock()
			if ok {
				s.stats.Lock()
				s.stats.CacheHits++
				s.stats.Unlock()
				w.Header().Set("X-Source", "Cache")
				w.Header().Set("X-Served-By-Region", s.config.Region)
				http.Redirect(w, r, urlVal, http.StatusFound)
				return
			}
		}

		// 2. Try Local Mock DB Replica
		if !failures.ReplicaDB {
			s.mockStore.mu.RLock()
			urlVal, ok := s.mockStore.replicaDB[code]
			s.mockStore.mu.RUnlock()
			if ok {
				source = "Local Replica DB"
				s.stats.Lock()
				s.stats.ReplicaHits++
				s.stats.Unlock()

				// Backfill local cache asynchronously
				if !failures.Redis {
					s.mockStore.mu.Lock()
					s.mockStore.cache[code] = urlVal
					s.mockStore.mu.Unlock()
				}

				w.Header().Set("X-Source", source)
				w.Header().Set("X-Served-By-Region", s.config.Region)
				http.Redirect(w, r, urlVal, http.StatusFound)
				return
			}
		}

		// 3. Fallback to Primary DB Coordinator (Cross-region read)
		if !failures.PrimaryDB && !s.config.IsPrimary {
			fallbackURL := fmt.Sprintf("%s/api/mock/primary?code=%s", s.config.RouterURL, code)
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(fallbackURL)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var data struct {
						URL string `json:"url"`
					}
					if json.NewDecoder(resp.Body).Decode(&data) == nil {
						source = "Primary DB Fallback"
						s.stats.Lock()
						s.stats.PrimaryFallback++
						s.stats.Unlock()

						// Backfill local cache and replica
						s.mockStore.mu.Lock()
						if !failures.Redis {
							s.mockStore.cache[code] = data.URL
						}
						if !failures.ReplicaDB {
							s.mockStore.replicaDB[code] = data.URL
						}
						s.mockStore.mu.Unlock()

						w.Header().Set("X-Source", source)
						w.Header().Set("X-Served-By-Region", s.config.Region)
						w.Header().Set("X-Database-Fallback", "true")
						http.Redirect(w, r, data.URL, http.StatusFound)
						return
					}
				} else if resp.StatusCode == http.StatusNotFound {
					s.respondWithError(w, http.StatusNotFound, "URL not found")
					return
				}
			}
		}

		// Check if it exists globally to determine if it's a datastore issue or a real 404
		existsURL := fmt.Sprintf("%s/api/mock/primary?code=%s", s.config.RouterURL, code)
		client := &http.Client{Timeout: 1 * time.Second}
		resp, err := client.Get(existsURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			s.respondWithError(w, http.StatusServiceUnavailable, "Data stores unavailable (simulated failures)")
		} else {
			s.respondWithError(w, http.StatusNotFound, "URL not found")
		}
		return
	}

	// --- Real Mode Resolution ---
	var err error

	// 1. Try Redis Cache
	if !failures.Redis && s.redis != nil {
		longURL, err = s.redis.Get(s.ctx, "url:"+code).Result()
		if err == nil {
			s.stats.Lock()
			s.stats.CacheHits++
			s.stats.Unlock()
			w.Header().Set("X-Source", "Cache")
			w.Header().Set("X-Served-By-Region", s.config.Region)
			http.Redirect(w, r, longURL, http.StatusFound)
			return
		}
		if err != redis.Nil {
			log.Printf("[%s] Redis error: %v", s.config.Region, err)
		}
	}

	// 2. Try Local DB Replica
	if !failures.ReplicaDB && s.replicaDB != nil {
		query := `SELECT long_url FROM urls WHERE code = $1`
		err = s.replicaDB.QueryRowContext(s.ctx, query, code).Scan(&longURL)
		if err == nil {
			found = true
			source = "Local Replica DB"
			s.stats.Lock()
			s.stats.ReplicaHits++
			s.stats.Unlock()
			
			// Backfill cache
			if !failures.Redis && s.redis != nil {
				go s.redis.Set(s.ctx, "url:"+code, longURL, 10*time.Minute)
			}
		} else if err != sql.ErrNoRows {
			log.Printf("[%s] Replica DB error: %v", s.config.Region, err)
		}
	}

	// 3. Fallback to Primary DB (Cross-region read)
	if !found && !failures.PrimaryDB && s.primaryDB != nil && !s.config.IsPrimary {
		query := `SELECT long_url FROM urls WHERE code = $1`
		err = s.primaryDB.QueryRowContext(s.ctx, query, code).Scan(&longURL)
		if err == nil {
			found = true
			source = "Primary DB Fallback"
			s.stats.Lock()
			s.stats.PrimaryFallback++
			s.stats.Unlock()
			
			// Backfill cache
			if !failures.Redis && s.redis != nil {
				go s.redis.Set(s.ctx, "url:"+code, longURL, 10*time.Minute)
			}
		}
	}

	if found {
		w.Header().Set("X-Source", source)
		w.Header().Set("X-Served-By-Region", s.config.Region)
		if source == "Primary DB Fallback" {
			w.Header().Set("X-Database-Fallback", "true")
		}
		http.Redirect(w, r, longURL, http.StatusFound)
	} else if err == sql.ErrNoRows {
		s.respondWithError(w, http.StatusNotFound, "URL not found")
	} else {
		s.respondWithError(w, http.StatusServiceUnavailable, "Data stores unavailable (simulated failures or network connection down)")
	}
}

// --- Mock API handlers ---

func (s *AppServer) handleMockReplicaSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var data struct {
		Code string `json:"code"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mockStore.mu.Lock()
	s.mockStore.replicaDB[data.Code] = data.URL
	s.mockStore.mu.Unlock()

	log.Printf("[%s] Mock database replica received sync for code %s", s.config.Region, data.Code)
	w.WriteHeader(http.StatusOK)
}

func (s *AppServer) handleMockCacheSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var data struct {
		Code string `json:"code"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mockStore.mu.Lock()
	s.mockStore.cache[data.Code] = data.URL
	s.mockStore.mu.Unlock()

	log.Printf("[%s] Mock cache received sync/backfill for code %s", s.config.Region, data.Code)
	w.WriteHeader(http.StatusOK)
}

// handleHealth returns status of all regional components
func (s *AppServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	failures := s.failures
	s.mu.RUnlock()

	status := "healthy"
	redisStatus := "UP"
	replicaStatus := "UP"
	primaryStatus := "UP"

	if s.config.MockMode {
		if failures.Redis {
			redisStatus = "DOWN"
		}
		if failures.ReplicaDB {
			replicaStatus = "DOWN"
		}
		if failures.PrimaryDB {
			primaryStatus = "DOWN"
		}
	} else {
		// Assess Redis Health
		if failures.Redis || s.redis == nil {
			redisStatus = "DOWN"
		} else if err := s.redis.Ping(s.ctx).Err(); err != nil {
			redisStatus = "DOWN"
		}

		// Assess Replica DB Health
		if failures.ReplicaDB || s.replicaDB == nil {
			replicaStatus = "DOWN"
		} else if err := s.replicaDB.Ping(); err != nil {
			replicaStatus = "DOWN"
		}

		// Assess Primary DB Health
		if failures.PrimaryDB || (s.primaryDB == nil && !s.config.IsPrimary) {
			primaryStatus = "DOWN"
		} else if s.primaryDB != nil {
			if err := s.primaryDB.Ping(); err != nil {
				primaryStatus = "DOWN"
			}
		}
	}

	// Overloaded system state
	if redisStatus == "DOWN" && replicaStatus == "DOWN" && primaryStatus == "DOWN" {
		status = "unhealthy"
	} else if replicaStatus == "DOWN" || redisStatus == "DOWN" {
		status = "degraded"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"region":       s.config.Region,
		"status":       status,
		"redis":        redisStatus,
		"replica_db":   replicaStatus,
		"primary_db":   primaryStatus,
		"is_primary":   s.config.IsPrimary,
		"simulated":    failures,
	})
}

// handleStats returns execution metrics
func (s *AppServer) handleStats(w http.ResponseWriter, r *http.Request) {
	s.stats.Lock()
	defer s.stats.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.stats)
}

// handleSimulateFail toggles a failure state
func (s *AppServer) handleSimulateFail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Component string `json:"component"` // "redis", "replica_db", "primary_db", "all"
		Status    string `json:"status"`    // "down", "up"
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	s.mu.Lock()
	val := (req.Status == "down")
	switch req.Component {
	case "redis":
		s.failures.Redis = val
	case "replica_db":
		s.failures.ReplicaDB = val
	case "primary_db":
		s.failures.PrimaryDB = val
	case "all":
		s.failures.Redis = val
		s.failures.ReplicaDB = val
		s.failures.PrimaryDB = val
	default:
		s.mu.Unlock()
		s.respondWithError(w, http.StatusBadRequest, "Invalid component. Choose redis, replica_db, primary_db, or all.")
		return
	}
	s.mu.Unlock()

	log.Printf("[%s] Simulated failure updated: %s -> %s", s.config.Region, req.Component, req.Status)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"region":  s.config.Region,
		"success": true,
		"message": fmt.Sprintf("Set %s state to %s", req.Component, req.Status),
	})
}

// handleSimulateReset clears all failure states and metrics
func (s *AppServer) handleSimulateReset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.failures = Failures{}
	s.mu.Unlock()

	s.stats.Lock()
	s.stats.CacheHits = 0
	s.stats.ReplicaHits = 0
	s.stats.PrimaryFallback = 0
	s.stats.TotalRequests = 0
	s.stats.Unlock()

	// Clear cache if active
	if s.config.MockMode {
		s.mockStore.mu.Lock()
		s.mockStore.cache = make(map[string]string)
		s.mockStore.replicaDB = make(map[string]string)
		// If primary, it is synced again when writes are made
		s.mockStore.mu.Unlock()
	} else if s.redis != nil {
		s.redis.FlushAll(s.ctx)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"region":  s.config.Region,
		"success": true,
		"message": "Reset all failure states, flushed cache, and cleared stats.",
	})
}

// --- Helpers ---

func (s *AppServer) incrementRequests() {
	s.stats.Lock()
	s.stats.TotalRequests++
	s.stats.Unlock()
}

func (s *AppServer) respondWithError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func generateShortCode(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	rand.Seed(time.Now().UnixNano())
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}
