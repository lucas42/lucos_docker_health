package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

func getEnvRequired(key string) string {
	val := os.Getenv(key)
	if val == "" {
		log.Fatalf("Required environment variable %s is not set", key)
	}
	return val
}

type statusReport struct {
	System    string `json:"system"`
	Frequency int    `json:"frequency"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
}

func checkHealth(ctx context.Context, dockerClient *client.Client) (bool, string) {
	containers, err := dockerClient.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return false, fmt.Sprintf("Failed to list containers: %v", err)
	}

	var unhealthy []string
	for _, c := range containers {
		info, err := dockerClient.ContainerInspect(ctx, c.ID)
		if err != nil {
			log.Printf("Warning: failed to inspect container %s: %v", c.ID[:12], err)
			continue
		}
		if info.State.Health == nil || info.State.Health.Status == "none" {
			// No healthcheck configured — skip
			continue
		}
		if info.State.Health.Status == "unhealthy" {
			name := c.ID[:12]
			if len(c.Names) > 0 {
				name = strings.TrimPrefix(c.Names[0], "/")
			}
			unhealthy = append(unhealthy, name)
		}
	}

	if len(unhealthy) > 0 {
		return false, "Unhealthy containers: " + strings.Join(unhealthy, ", ")
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
	system := getEnvRequired("SYSTEM")
	scheduleTrackerURL := getEnvRequired("SCHEDULE_TRACKER_URL")

	frequency := 60
	if val := os.Getenv("REPORT_FREQUENCY"); val != "" {
		parsed, err := strconv.Atoi(val)
		if err != nil {
			log.Fatalf("Invalid REPORT_FREQUENCY value %q: %v", val, err)
		}
		frequency = parsed
	}

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	httpClient := &http.Client{Timeout: 10 * time.Second}

	log.Printf("Starting lucos_docker_health — system=%s, frequency=%ds", system, frequency)

	ticker := time.NewTicker(time.Duration(frequency) * time.Second)
	defer ticker.Stop()

	// Run once immediately, then on each tick
	ctx := context.Background()
	healthy, message := checkHealth(ctx, dockerClient)
	reportStatus(httpClient, scheduleTrackerURL, system, frequency, healthy, message)

	for range ticker.C {
		healthy, message = checkHealth(ctx, dockerClient)
		reportStatus(httpClient, scheduleTrackerURL, system, frequency, healthy, message)
	}
}
