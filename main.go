package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

const heartbeatFile = "/tmp/heartbeat"

func getEnvRequired(key string) string {
	val := os.Getenv(key)
	if val == "" {
		log.Fatalf("Required environment variable %s is not set", key)
	}
	return val
}

func getFrequency() int {
	frequency := 60
	if val := os.Getenv("REPORT_FREQUENCY"); val != "" {
		parsed, err := strconv.Atoi(val)
		if err != nil {
			log.Fatalf("Invalid REPORT_FREQUENCY value %q: %v", val, err)
		}
		frequency = parsed
	}
	return frequency
}

func runHealthcheck() {
	frequency := getFrequency()
	data, err := os.ReadFile(heartbeatFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Healthcheck failed: cannot read heartbeat file: %v\n", err)
		os.Exit(1)
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Healthcheck failed: invalid heartbeat timestamp: %v\n", err)
		os.Exit(1)
	}
	age := time.Since(time.Unix(ts, 0))
	threshold := time.Duration(frequency*2) * time.Second
	if age > threshold {
		fmt.Fprintf(os.Stderr, "Healthcheck failed: last report was %s ago (threshold %s)\n", age.Round(time.Second), threshold)
		os.Exit(1)
	}
	os.Exit(0)
}

func writeHeartbeat() {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	if err := os.WriteFile(heartbeatFile, []byte(ts), 0644); err != nil {
		log.Printf("Warning: failed to write heartbeat file: %v", err)
	}
}

type statusReport struct {
	System    string `json:"system"`
	Frequency int    `json:"frequency"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
}

const stuckStartingThreshold = 5 * time.Minute

func checkHealth(ctx context.Context, dockerClient *client.Client) (bool, string) {
	result, err := dockerClient.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return false, fmt.Sprintf("Failed to list containers: %v", err)
	}

	var unhealthy []string
	var stuckStarting []string
	for _, c := range result.Items {
		inspectCtx, inspectCancel := context.WithTimeout(ctx, 5*time.Second)
		info, err := dockerClient.ContainerInspect(inspectCtx, c.ID, client.ContainerInspectOptions{})
		inspectCancel()
		if err != nil {
			if errdefs.IsNotFound(err) || errors.Is(err, context.DeadlineExceeded) {
				// Container was removed between list and inspect (e.g. during a deploy
				// wave) — skip silently; the next cycle will have a fresh list.
				continue
			}
			log.Printf("Warning: failed to inspect container %s: %v", c.ID[:12], err)
			continue
		}
		if info.Container.State.Health == nil || info.Container.State.Health.Status == "none" {
			// No healthcheck configured — skip
			continue
		}
		name := c.ID[:12]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		switch info.Container.State.Health.Status {
		case "unhealthy":
			unhealthy = append(unhealthy, name)
		case "starting":
			startedAt, err := time.Parse(time.RFC3339Nano, info.Container.State.StartedAt)
			if err != nil {
				log.Printf("Warning: failed to parse StartedAt for container %s: %v", name, err)
				continue
			}
			if time.Since(startedAt) > stuckStartingThreshold {
				stuckStarting = append(stuckStarting, name)
			}
		}
	}

	var parts []string
	if len(unhealthy) > 0 {
		parts = append(parts, "Unhealthy containers: "+strings.Join(unhealthy, ", "))
	}
	if len(stuckStarting) > 0 {
		parts = append(parts, "Stuck starting: "+strings.Join(stuckStarting, ", "))
	}
	if len(parts) > 0 {
		return false, strings.Join(parts, ". ")
	}
	return true, ""
}

func reportStatus(httpClient *http.Client, url, system string, frequency int, healthy bool, message string) {
	report := statusReport{
		System:    system,
		Frequency: frequency,
		Status:    "success",
	}
	if !healthy {
		report.Status = "error"
		report.Message = message
	}

	body, err := json.Marshal(report)
	if err != nil {
		log.Printf("Failed to marshal report: %v", err)
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("Failed to create request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", system)

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("Failed to report status to schedule_tracker: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("schedule_tracker returned unexpected status: %d", resp.StatusCode)
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		runHealthcheck()
	}

	systemBase := getEnvRequired("SYSTEM")
	hostDomain := getEnvRequired("HOSTDOMAIN")
	hostPrefix := strings.SplitN(hostDomain, ".", 2)[0]
	system := systemBase + "_" + hostPrefix
	scheduleTrackerURL := getEnvRequired("SCHEDULE_TRACKER_ENDPOINT")
	frequency := getFrequency()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Fatalf("Failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	httpClient := &http.Client{Timeout: 10 * time.Second}

	log.Printf("Starting lucos_docker_health — system=%s, frequency=%ds", system, frequency)

	ticker := time.NewTicker(time.Duration(frequency) * time.Second)
	defer ticker.Stop()

	runCheck := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		healthy, message := checkHealth(ctx, dockerClient)
		reportStatus(httpClient, scheduleTrackerURL, system, frequency, healthy, message)
		writeHeartbeat()
	}

	// Run once immediately, then on each tick
	runCheck()
	for range ticker.C {
		runCheck()
	}
}
