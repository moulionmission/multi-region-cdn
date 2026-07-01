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

	// Verify pre-compiled binaries exist
	if _, err := os.Stat("app_server"); os.IsNotExist(err) {
		log.Println("[Simulator] Warning: 'app_server' binary not found. Falling back to Go source compile...")
		// We fallback to compiling locally if binaries aren't built, ensuring local developer friendliness
		compileBinary("cmd/app/main.go", "app_server")
	}
	if _, err := os.Stat("router_server"); os.IsNotExist(err) {
		log.Println("[Simulator] Warning: 'router_server' binary not found. Falling back to Go source compile...")
		compileBinary("cmd/router/main.go", "router_server")
	}

	processes := []*Subprocess{
		// 1. US-East Regional Server
		{
			Name: "US-EAST (Primary)",
			Cmd: createCmd("./app_server", map[string]string{
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
			Cmd: createCmd("./app_server", map[string]string{
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
			Cmd: createCmd("./app_server", map[string]string{
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
			Cmd: createCmd("./router_server", map[string]string{
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
			p.Cmd.Process.Signal(syscall.SIGINT)
			
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

func compileBinary(source, output string) {
	cmd := exec.Command("go", "build", "-o", output, source)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("[Simulator] Failed to compile fallback binary for %s: %v", source, err)
	}
	log.Printf("[Simulator] Fallback compilation of %s completed successfully.", output)
}

func createCmd(binaryPath string, env map[string]string) *exec.Cmd {
	cmd := exec.Command(binaryPath)
	
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

	go streamLogs(fmt.Sprintf("[%s STDOUT]", p.Name), stdout)
	go streamLogs(fmt.Sprintf("[%s STDERR]", p.Name), stderr)

	return nil
}

func streamLogs(prefix string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		log.Printf("%s %s", prefix, scanner.Text())
	}
}
