package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RegionInfo stores status and endpoints for a region
type RegionInfo struct {
	Name        string `json:"name"`
	Endpoint    string `json:"endpoint"`
	IsPrimary   bool   `json:"is_primary"`
	Healthy     bool   `json:"healthy"`
	LastChecked string `json:"last_checked"`
}

type GlobalRouter struct {
	port          string
	regions       map[string]*RegionInfo
	proxies       map[string]*httputil.ReverseProxy
	mu            sync.RWMutex
	webDir        string
	latencyMatrix map[string]map[string]int // simulated latency in ms

	// Global mock DB coordinator state
	mockPrimaryDB map[string]string
	mockMu        sync.RWMutex
}

func main() {
	port := getEnv("PORT", "8080")
	webDir := getEnv("WEB_DIR", "./web")

	// Get region endpoints (either local ports or container names)
	usEastURL := getEnv("US_EAST_URL", "http://localhost:8081")
	usWestURL := getEnv("US_WEST_URL", "http://localhost:8082")
	euWestURL := getEnv("EU_WEST_URL", "http://localhost:8083")

	router := &GlobalRouter{
		port:          port,
		webDir:        webDir,
		regions:       make(map[string]*RegionInfo),
		proxies:       make(map[string]*httputil.ReverseProxy),
		mockPrimaryDB: make(map[string]string),
		latencyMatrix: map[string]map[string]int{
			"us-east": {"us-east": 15, "us-west": 75, "eu-west": 110},
			"us-west": {"us-east": 75, "us-west": 10, "eu-west": 180},
			"eu-west": {"us-east": 110, "us-west": 180, "eu-west": 12},
		},
	}

	router.registerRegion("us-east", usEastURL, true)
	router.registerRegion("us-west", usWestURL, false)
	router.registerRegion("eu-west", euWestURL, false)

	// Start active health checker background goroutine
	go router.startHealthChecker(2 * time.Second)

	// Register routes
	mux := http.NewServeMux()
	mux.HandleFunc("/api/regions", router.handleAPIRegions)
	mux.HandleFunc("/api/simulate/fail", router.handleAPISimulateFail)
	mux.HandleFunc("/api/simulate/reset", router.handleAPISimulateReset)
	mux.HandleFunc("/api/request", router.handleAPIRequest) // Proxy tester
	
	// Mock Coordinator Endpoints
	mux.HandleFunc("/api/mock/write", router.handleMockCoordinatorWrite)
	mux.HandleFunc("/api/mock/primary", router.handleMockCoordinatorRead)
	
	mux.HandleFunc("/dashboard/", router.handleDashboardStatic)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", router.handleGlobalRoute) // Main proxy and shortener entry point

	log.Printf("[Router] Global Router listening on port %s...", port)
	log.Fatalf("[Router] Start failed: %v", http.ListenAndServe(":"+port, mux))
}

func (gr *GlobalRouter) registerRegion(name, endpoint string, isPrimary bool) {
	u, err := url.Parse(endpoint)
	if err != nil {
		log.Fatalf("Invalid region endpoint %s for region %s: %v", endpoint, name, err)
	}

	gr.regions[name] = &RegionInfo{
		Name:      name,
		Endpoint:  endpoint,
		IsPrimary: isPrimary,
		Healthy:   true,
	}

	// Custom reverse proxy that injects latency and failover headers
	proxy := httputil.NewSingleHostReverseProxy(u)
	
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Header.Set("X-Forwarded-Host", req.Header.Get("Host"))
		req.Header.Set("X-Forwarded-For", req.RemoteAddr)
	}

	gr.proxies[name] = proxy
}

func (gr *GlobalRouter) startHealthChecker(interval time.Duration) {
	client := &http.Client{Timeout: 1 * time.Second}
	ticker := time.NewTicker(interval)

	for range ticker.C {
		gr.mu.Lock()
		for name, reg := range gr.regions {
			healthURL := fmt.Sprintf("%s/health", reg.Endpoint)
			resp, err := client.Get(healthURL)
			
			wasHealthy := reg.Healthy
			isHealthy := false
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusInternalServerError {
					isHealthy = true
				}
			}

			reg.Healthy = isHealthy
			reg.LastChecked = time.Now().Format(time.RFC3339)

			if wasHealthy != isHealthy {
				log.Printf("[Router] Health changed for region %s: Healthy=%v", name, isHealthy)
			}
		}
		gr.mu.Unlock()
	}
}

// handleGlobalRoute routes write requests (POST /urls) to US-East (Primary)
// and read requests (GET /:code) to the target region, with failover
func (gr *GlobalRouter) handleGlobalRoute(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		http.Redirect(w, r, "/dashboard/", http.StatusMovedPermanently)
		return
	}

	if r.Method == http.MethodPost && r.URL.Path == "/urls" {
		gr.proxyToRegion(w, r, "us-east", false)
		return
	}

	clientRegion := strings.ToLower(r.Header.Get("X-Client-Region"))
	if clientRegion == "" {
		clientRegion = "us-east"
	}

	gr.mu.RLock()
	reg, ok := gr.regions[clientRegion]
	gr.mu.RUnlock()

	if !ok {
		clientRegion = "us-east"
	}

	gr.mu.RLock()
	healthy := reg != nil && reg.Healthy
	gr.mu.RUnlock()

	if healthy {
		gr.proxyToRegion(w, r, clientRegion, false)
	} else {
		failoverRegion := gr.findFailoverRegion(clientRegion)
		if failoverRegion == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "All regional instances are currently offline."})
			return
		}
		
		log.Printf("[Router] Failover: Region %s is down. Rerouting request to %s.", clientRegion, failoverRegion)
		gr.proxyToRegion(w, r, failoverRegion, true)
	}
}

func (gr *GlobalRouter) proxyToRegion(w http.ResponseWriter, r *http.Request, region string, isFailover bool) {
	gr.mu.RLock()
	proxy, hasProxy := gr.proxies[region]
	clientRegion := strings.ToLower(r.Header.Get("X-Client-Region"))
	gr.mu.RUnlock()

	if !hasProxy {
		http.Error(w, "Proxy not configured", http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Original-Region", clientRegion)
	w.Header().Set("X-Served-By-Region", region)
	if isFailover {
		w.Header().Set("X-Failover-Triggered", "true")
	} else {
		w.Header().Set("X-Failover-Triggered", "false")
	}

	if clientRegion != "" {
		networkLatency := gr.getLatency(clientRegion, region)
		time.Sleep(time.Duration(networkLatency) * time.Millisecond)
		w.Header().Set("X-Network-Latency-Ms", fmt.Sprintf("%d", networkLatency))
	}

	proxy.ServeHTTP(w, r)
}

func (gr *GlobalRouter) findFailoverRegion(failedRegion string) string {
	gr.mu.RLock()
	defer gr.mu.RUnlock()

	var priority []string
	switch failedRegion {
	case "us-west":
		priority = []string{"us-east", "eu-west"}
	case "eu-west":
		priority = []string{"us-east", "us-west"}
	case "us-east":
		priority = []string{"us-west", "eu-west"}
	default:
		priority = []string{"us-east", "us-west", "eu-west"}
	}

	for _, name := range priority {
		if reg, ok := gr.regions[name]; ok && reg.Healthy {
			return name
		}
	}
	return ""
}

func (gr *GlobalRouter) getLatency(clientRegion, targetRegion string) int {
	if latencies, ok := gr.latencyMatrix[clientRegion]; ok {
		if lat, exists := latencies[targetRegion]; exists {
			return lat
		}
	}
	return 0
}

// --- Mock DB Coordinator Handlers ---

func (gr *GlobalRouter) handleMockCoordinatorWrite(w http.ResponseWriter, r *http.Request) {
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

	gr.mockMu.Lock()
	if _, exists := gr.mockPrimaryDB[data.Code]; exists {
		gr.mockMu.Unlock()
		w.WriteHeader(http.StatusConflict)
		return
	}
	gr.mockPrimaryDB[data.Code] = data.URL
	gr.mockMu.Unlock()

	log.Printf("[Router Mock Coordinator] Stored short URL '%s' -> '%s' in primary mock DB.", data.Code, data.URL)

	// Propagate replication writes asynchronously
	// us-east (primary app) receives it immediately
	// us-west (replica app) replicates with 1.5 seconds lag
	// eu-west (replica app) replicates with 3.0 seconds lag
	go gr.replicateToMockApp("us-east", data.Code, data.URL, 0)
	go gr.replicateToMockApp("us-west", data.Code, data.URL, 1500*time.Millisecond)
	go gr.replicateToMockApp("eu-west", data.Code, data.URL, 3000*time.Millisecond)

	w.WriteHeader(http.StatusCreated)
}

func (gr *GlobalRouter) handleMockCoordinatorRead(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "code parameter required", http.StatusBadRequest)
		return
	}

	gr.mockMu.RLock()
	urlVal, exists := gr.mockPrimaryDB[code]
	gr.mockMu.RUnlock()

	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": urlVal})
}

func (gr *GlobalRouter) replicateToMockApp(region, code, longURL string, lag time.Duration) {
	time.Sleep(lag)
	
	gr.mu.RLock()
	reg, ok := gr.regions[region]
	gr.mu.RUnlock()

	if !ok || !reg.Healthy {
		log.Printf("[Router Mock Coordinator] Replication skipped for %s: region offline/not configured", region)
		return
	}

	payload, _ := json.Marshal(map[string]string{
		"code": code,
		"url":  longURL,
	})

	client := &http.Client{Timeout: 1 * time.Second}
	syncURL := fmt.Sprintf("%s/api/mock/replica/sync", reg.Endpoint)
	resp, err := client.Post(syncURL, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		log.Printf("[Router Mock Coordinator] Replication failed to %s: %v", region, err)
		return
	}
	resp.Body.Close()
	log.Printf("[Router Mock Coordinator] Replicated '%s' to %s after %v lag", code, region, lag)
}

// --- Dashboard & API Handlers ---

func (gr *GlobalRouter) handleAPIRegions(w http.ResponseWriter, r *http.Request) {
	gr.mu.RLock()
	defer gr.mu.RUnlock()

	client := &http.Client{Timeout: 1 * time.Second}
	type RegionDetail struct {
		Name         string                 `json:"name"`
		Endpoint     string                 `json:"endpoint"`
		IsPrimary    bool                   `json:"is_primary"`
		Healthy      bool                   `json:"healthy"`
		LastChecked  string                 `json:"last_checked"`
		HealthDetail map[string]interface{} `json:"health_detail,omitempty"`
		Stats        map[string]interface{} `json:"stats,omitempty"`
	}

	details := make(map[string]RegionDetail)

	for name, reg := range gr.regions {
		detail := RegionDetail{
			Name:        reg.Name,
			Endpoint:    reg.Endpoint,
			IsPrimary:   reg.IsPrimary,
			Healthy:     reg.Healthy,
			LastChecked: reg.LastChecked,
		}

		if reg.Healthy {
			resp, err := client.Get(fmt.Sprintf("%s/health", reg.Endpoint))
			if err == nil {
				defer resp.Body.Close()
				var h map[string]interface{}
				if json.NewDecoder(resp.Body).Decode(&h) == nil {
					detail.HealthDetail = h
				}
			}

			resp2, err := client.Get(fmt.Sprintf("%s/stats", reg.Endpoint))
			if err == nil {
				defer resp2.Body.Close()
				var s map[string]interface{}
				if json.NewDecoder(resp2.Body).Decode(&s) == nil {
					detail.Stats = s
				}
			}
		}

		details[name] = detail
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(details)
}

func (gr *GlobalRouter) handleAPISimulateFail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Region    string `json:"region"`
		Component string `json:"component"`
		Status    string `json:"status"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	gr.mu.RLock()
	reg, ok := gr.regions[req.Region]
	gr.mu.RUnlock()

	if !ok {
		http.Error(w, "Region not found", http.StatusNotFound)
		return
	}

	jsonBytes, _ := json.Marshal(map[string]string{
		"component": req.Component,
		"status":    req.Status,
	})

	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Post(fmt.Sprintf("%s/simulate/fail", reg.Endpoint), "application/json", bytes.NewBuffer(jsonBytes))
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to communicate with region: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (gr *GlobalRouter) handleAPISimulateReset(w http.ResponseWriter, r *http.Request) {
	gr.mu.RLock()
	
	// Reset Router's mock DB
	gr.mockMu.Lock()
	gr.mockPrimaryDB = make(map[string]string)
	gr.mockMu.Unlock()

	client := &http.Client{Timeout: 1 * time.Second}
	for name, reg := range gr.regions {
		if !reg.Healthy {
			continue
		}
		resp, err := client.Post(fmt.Sprintf("%s/simulate/reset", reg.Endpoint), "application/json", nil)
		if err != nil {
			log.Printf("[Router] Failed to reset region %s: %v", name, err)
		} else {
			resp.Body.Close()
		}
	}
	gr.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "Triggered failure reset across all regions"})
}

func (gr *GlobalRouter) handleAPIRequest(w http.ResponseWriter, r *http.Request) {
	clientRegion := r.URL.Query().Get("client_region")
	method := r.Method
	path := r.URL.Query().Get("path")
	
	targetURL := fmt.Sprintf("http://localhost:%s%s", gr.port, path)
	
	var req *http.Request
	var err error
	
	if method == http.MethodPost {
		// Read body from request
		bodyBytes, _ := io.ReadAll(r.Body)
		req, err = http.NewRequest(method, targetURL, bytes.NewBuffer(bodyBytes))
	} else {
		req, err = http.NewRequest(method, targetURL, nil)
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	req.Header.Set("X-Client-Region", clientRegion)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, next []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	startTime := time.Now()
	resp, err := client.Do(req)
	duration := time.Since(startTime)

	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status_code":       resp.StatusCode,
		"served_by":         resp.Header.Get("X-Served-By-Region"),
		"original_region":   resp.Header.Get("X-Original-Region"),
		"failover":          resp.Header.Get("X-Failover-Triggered"),
		"db_fallback":       resp.Header.Get("X-Database-Fallback"),
		"source":            resp.Header.Get("X-Source"),
		"network_latency":   resp.Header.Get("X-Network-Latency-Ms"),
		"total_latency_ms":  duration.Milliseconds(),
		"redirect_url":      resp.Header.Get("Location"),
		"response_body":     string(bodyBytes),
	})
}

func (gr *GlobalRouter) handleDashboardStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/dashboard/" || path == "/dashboard" {
		path = "/dashboard/index.html"
	}

	localPath := strings.TrimPrefix(path, "/dashboard/")
	if localPath == "" {
		localPath = "index.html"
	}

	fullPath := filepath.Join(gr.webDir, localPath)

	cleanedFullPath := filepath.Clean(fullPath)
	cleanedWebDir := filepath.Clean(gr.webDir)
	if !strings.HasPrefix(cleanedFullPath, cleanedWebDir) {
		http.Error(w, "Access Denied", http.StatusForbidden)
		return
	}

	http.ServeFile(w, r, fullPath)
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}
