package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

type Subprocess struct {
	Name string
	Cmd  *exec.Cmd
}

func main() {
	log.Println("[Simulator] Initializing APEX-CDN Multi-Region Mock Mesh...")

	// Verify cmd files exist
	if _, err := os.Stat("cmd/app/main.go"); os.IsNotExist(err) {
		log.Fatal("Could not find 'cmd/app/main.go'. Please run the simulator from the project root directory /Users/b.v.ramana/Desktop/multi-region-cdn")
	}

	processes := []*Subprocess{
		// 1. US-East Regional Server
		{
			Name: "US-EAST (Primary)",
			Cmd: createCmd("cmd/app/main.go", map[string]string{
				"REGION":     "us-east",
				"PORT":       "8081",
				"IS_PRIMARY": "true",
				"MOCK_MODE":  "true",
				"ROUTER_URL": "http://localhost:8080",
			}),
		},
		// 2. US-West Regional Server
		{
			Name: "US-WEST (Replica)",
			Cmd: createCmd("cmd/app/main.go", map[string]string{
				"REGION":     "us-west",
				"PORT":       "8082",
				"IS_PRIMARY": "false",
				"MOCK_MODE":  "true",
				"ROUTER_URL": "http://localhost:8080",
			}),
		},
		// 3. EU-West Regional Server
		{
			Name: "EU-WEST (Replica)",
			Cmd: createCmd("cmd/app/main.go", map[string]string{
				"REGION":     "eu-west",
				"PORT":       "8083",
				"IS_PRIMARY": "false",
				"MOCK_MODE":  "true",
				"ROUTER_URL": "http://localhost:8080",
			}),
		},
		// 4. Global Load Balancer Router
		{
			Name: "GLOBAL ROUTER",
			Cmd: createCmd("cmd/router/main.go", map[string]string{
				"PORT":        "8080",
				"US_EAST_URL": "http://localhost:8081",
				"US_WEST_URL": "http://localhost:8082",
				"EU_WEST_URL": "http://localhost:8083",
				"WEB_DIR":     "./web",
			}),
		},
	}

	// Channel to capture OS interrupts (Ctrl+C)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start all processes
	for _, p := range processes {
		log.Printf("[Simulator] Starting %s service...", p.Name)
		if err := startSubprocess(p); err != nil {
			log.Fatalf("[Simulator] Failed to start %s: %v", p.Name, err)
		}
	}

	log.Println("\n============================================================")
	log.Println("  APEX-CDN SIMULATOR IS ONLINE!")
	log.Println("  Open http://localhost:8080/dashboard/ in your browser.")
	log.Println("  Press Ctrl+C to terminate all services.")
	log.Println("============================================================\n")

	// Wait for terminate signal
	<-sigChan
	log.Println("\n[Simulator] Terminate signal received. Shutting down all regional mesh components...")

	// Clean shutdown of all subprocesses
	for _, p := range processes {
		if p.Cmd.Process != nil {
			log.Printf("[Simulator] Stopping %s...", p.Name)
			// Send SIGINT
			p.Cmd.Process.Signal(syscall.SIGINT)
			
			// Wait for exit, force kill if hangs after 2s
			done := make(chan error, 1)
			go func() {
				done <- p.Cmd.Wait()
			}()
			
			select {
			case <-done:
				// Exited clean
			case <-time.After(2 * time.Second):
				log.Printf("[Simulator] %s failed to stop in time. Force killing...", p.Name)
				p.Cmd.Process.Kill()
			}
		}
	}

	log.Println("[Simulator] All services stopped. Goodbye!")
}

func createCmd(sourcePath string, env map[string]string) *exec.Cmd {
	cmd := exec.Command("go", "run", sourcePath)
	
	// Inherit environment variables
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	return cmd
}

func startSubprocess(p *Subprocess) error {
	stdout, err := p.Cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := p.Cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := p.Cmd.Start(); err != nil {
		return err
	}

	// Stream logs in separate goroutines
	go streamLogs(fmt.Sprintf("[%s STDOUT]", p.Name), stdout)
	go streamLogs(fmt.Sprintf("[%s STDERR]", p.Name), stderr)

	return nil
}

func streamLogs(prefix string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		// Log stdout/stderr of subprocesses with custom prefix
		log.Printf("%s %s", prefix, scanner.Text())
	}
}
