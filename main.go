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
	JobName   string `json:"job_name"`
	Frequency int    `json:"frequency"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
}

const stuckStartingThreshold = 5 * time.Minute

// crashLoopStreakThreshold is how many consecutive polls must see a rising
// RestartCount before a container is flagged as crash-looping. Requiring two
// avoids flagging a fresh container after a deploy (RestartCount resets to 0)
// or a one-off crash-and-recover (bumps the count once).
const crashLoopStreakThreshold = 2

// crashLoopExitFlatPolls is how many consecutive polls with a flat RestartCount
// clear a flagged container, so restart backoff gaps don't drop the flag.
const crashLoopExitFlatPolls = 3

type restartState struct {
	lastCount    int
	risingStreak int
	flatStreak   int
	flagged      bool
}

// crashLoopDetector tracks each container's RestartCount across polls.
// A container whose healthcheck oscillates fast enough can defeat both the
// "unhealthy" and "stuck starting" checks — a restarting-every-few-seconds
// container never accumulates 5 minutes of "starting", and rarely holds
// "unhealthy" long enough to be sampled (lucas42/lucos_docker_health#108).
// RestartCount is monotonic, so comparing it between polls gives the same
// answer regardless of which instant is sampled.
type crashLoopDetector struct {
	state map[string]restartState
}

func newCrashLoopDetector() *crashLoopDetector {
	return &crashLoopDetector{state: make(map[string]restartState)}
}

// observe records this poll's RestartCount for containerID and reports
// whether it should be considered crash-looping.
func (d *crashLoopDetector) observe(containerID string, restartCount int) bool {
	prev, seen := d.state[containerID]
	next := restartState{lastCount: restartCount}
	if seen && restartCount > prev.lastCount {
		next.risingStreak = prev.risingStreak + 1
	} else if seen {
		next.flatStreak = prev.flatStreak + 1
	}
	next.flagged = next.risingStreak >= crashLoopStreakThreshold ||
		(prev.flagged && next.flatStreak < crashLoopExitFlatPolls)
	d.state[containerID] = next
	return next.flagged
}

// prune drops state for containers not seen in the current poll, so the map
// doesn't grow unbounded as containers are replaced across deploys.
func (d *crashLoopDetector) prune(present map[string]struct{}) {
	for id := range d.state {
		if _, ok := present[id]; !ok {
			delete(d.state, id)
		}
	}
}

// stuckPausedPolls is how many consecutive polls must see a container paused
// before it's flagged; three (≥2 min) clears lucos_backups' 120s quiesce watchdog.
const stuckPausedPolls = 3

// pausedDetector counts each container's consecutive paused polls, so a
// container left paused (e.g. by a failed backup quiesce) is still reported
// once synthetic "unhealthy" statuses are ignored (lucas42/lucos_docker_health#117).
type pausedDetector struct {
	streak map[string]int
}

func newPausedDetector() *pausedDetector {
	return &pausedDetector{streak: make(map[string]int)}
}

// observe records whether containerID is paused this poll and reports whether
// it has now been paused for stuckPausedPolls consecutive polls.
func (d *pausedDetector) observe(containerID string, paused bool) bool {
	if !paused {
		delete(d.streak, containerID)
		return false
	}
	d.streak[containerID]++
	return d.streak[containerID] >= stuckPausedPolls
}

func (d *pausedDetector) prune(present map[string]struct{}) {
	for id := range d.streak {
		if _, ok := present[id]; !ok {
			delete(d.streak, id)
		}
	}
}

// countsAsUnhealthy ignores the "unhealthy" Docker sets on a paused container
// (and keeps for one interval after unpause) without any probe having failed.
func countsAsUnhealthy(status string, failingStreak int) bool {
	return status == "unhealthy" && failingStreak > 0
}

// daemonConnLostPolls is how many consecutive polls must fail to reach the
// Docker daemon before the process exits so `restart: always` re-resolves the
// socket bind mount (a daemon restart leaves it pinned to a deleted inode).
const daemonConnLostPolls = 3

type daemonConnTracker struct {
	streak int
}

// observe records the outcome of a container list and reports whether the
// daemon has been unreachable for daemonConnLostPolls consecutive polls.
// Only connection failures count; timeouts and other errors reset the streak.
func (t *daemonConnTracker) observe(err error) bool {
	if err != nil && client.IsErrConnectionFailed(err) {
		t.streak++
	} else {
		t.streak = 0
	}
	return t.streak >= daemonConnLostPolls
}

func checkHealth(ctx context.Context, dockerClient *client.Client, detector *crashLoopDetector, paused *pausedDetector, conn *daemonConnTracker) (bool, string) {
	log.Printf("Listing containers...")
	listStart := time.Now()
	result, err := dockerClient.ContainerList(ctx, client.ContainerListOptions{})
	conn.observe(err)
	if err != nil {
		return false, fmt.Sprintf("Failed to list containers: %v", err)
	}
	log.Printf("Listed %d containers in %dms", len(result.Items), time.Since(listStart).Milliseconds())

	var unhealthy []string
	var stuckStarting []string
	var crashLooping []string
	var stuckPaused []string
	present := make(map[string]struct{})
	for _, c := range result.Items {
		// Seed from the list, not after inspect succeeds — a transient inspect
		// failure (e.g. context.DeadlineExceeded under daemon load, which a real
		// crash loop can itself cause) must not look like container removal and
		// prune this container's accumulating crash-loop streak.
		present[c.ID] = struct{}{}
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
		name := c.ID[:12]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		// Checked before the healthcheck skip, so it covers containers without one.
		if paused.observe(c.ID, info.Container.State.Paused) {
			stuckPaused = append(stuckPaused, name)
		}
		if info.Container.State.Health == nil || info.Container.State.Health.Status == "none" {
			// No healthcheck configured — skip
			continue
		}
		if detector.observe(c.ID, info.Container.RestartCount) {
			crashLooping = append(crashLooping, name)
		}
		switch info.Container.State.Health.Status {
		case "unhealthy":
			if countsAsUnhealthy(string(info.Container.State.Health.Status), info.Container.State.Health.FailingStreak) {
				unhealthy = append(unhealthy, name)
			}
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
	detector.prune(present)
	paused.prune(present)

	var parts []string
	if len(unhealthy) > 0 {
		parts = append(parts, "Unhealthy containers: "+strings.Join(unhealthy, ", "))
	}
	if len(crashLooping) > 0 {
		parts = append(parts, "Crash-looping (RestartCount rising): "+strings.Join(crashLooping, ", "))
	}
	if len(stuckStarting) > 0 {
		parts = append(parts, "Stuck starting: "+strings.Join(stuckStarting, ", "))
	}
	if len(stuckPaused) > 0 {
		parts = append(parts, "Stuck paused: "+strings.Join(stuckPaused, ", "))
	}
	if len(parts) > 0 {
		return false, strings.Join(parts, ". ")
	}
	return true, ""
}

func reportStatus(httpClient *http.Client, url, system, jobName string, frequency int, healthy bool, message string, age int) {
	report := statusReport{
		System:    system,
		JobName:   jobName,
		Frequency: frequency,
		Status:    "success",
	}
	if !healthy {
		log.Printf("Reporting unhealthy: %s", message)
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
	req.Header.Set("User-Agent", system+"_"+jobName)

	log.Printf("Heartbeat to %s: status=%s, age=%ds", url, report.Status, age)
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("Heartbeat failed: %v", err)
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

	system := getEnvRequired("SYSTEM")
	hostDomain := getEnvRequired("HOSTDOMAIN")
	jobName := strings.SplitN(hostDomain, ".", 2)[0]
	scheduleTrackerURL := getEnvRequired("SCHEDULE_TRACKER_ENDPOINT")
	frequency := getFrequency()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Fatalf("Failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	httpClient := &http.Client{Timeout: 10 * time.Second}

	log.Printf("Starting lucos_docker_health — system=%s, job_name=%s, frequency=%ds", system, jobName, frequency)

	ticker := time.NewTicker(time.Duration(frequency) * time.Second)
	defer ticker.Stop()

	detector := newCrashLoopDetector()
	pausedDet := newPausedDetector()
	conn := &daemonConnTracker{}
	runCheck := func() {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		healthy, message := checkHealth(ctx, dockerClient, detector, pausedDet, conn)
		reportStatus(httpClient, scheduleTrackerURL, system, jobName, frequency, healthy, message, int(time.Since(start).Seconds()))
		writeHeartbeat()
		if conn.streak >= daemonConnLostPolls {
			log.Printf("Docker daemon unreachable for %d consecutive checks; exiting so the container restarts and re-establishes the socket mount", conn.streak)
			os.Exit(1)
		}
	}

	// Run once immediately, then on each tick
	runCheck()
	for range ticker.C {
		runCheck()
	}
}
