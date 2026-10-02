package main

import "testing"

// TestPausedDetector: a quiesce pause (cleared within a poll or two) must not
// flag; a container left paused for stuckPausedPolls consecutive polls must.
func TestPausedDetector(t *testing.T) {
	tests := []struct {
		name    string
		polls   []bool // State.Paused observed on each successive poll
		flagged []bool
	}{
		{
			name:    "never paused is never flagged",
			polls:   []bool{false, false, false},
			flagged: []bool{false, false, false},
		},
		{
			name:    "flags on the third consecutive paused poll",
			polls:   []bool{true, true, true, true},
			flagged: []bool{false, false, true, true},
		},
		{
			name:    "a brief pause seen on two polls is not flagged",
			polls:   []bool{true, true, false, false},
			flagged: []bool{false, false, false, false},
		},
		{
			name:    "an unpaused poll resets the streak",
			polls:   []bool{true, true, false, true, true, true},
			flagged: []bool{false, false, false, false, false, true},
		},
		{
			name:    "unflags as soon as the container is unpaused",
			polls:   []bool{true, true, true, false},
			flagged: []bool{false, false, true, false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newPausedDetector()
			for i, p := range tt.polls {
				if got := d.observe("container-a", p); got != tt.flagged[i] {
					t.Errorf("poll %d (paused=%v): got flagged=%v, want %v", i, p, got, tt.flagged[i])
				}
			}
		})
	}
}

func TestPausedDetectorPrune(t *testing.T) {
	d := newPausedDetector()
	d.observe("container-a", true)
	d.observe("container-a", true)
	d.prune(map[string]struct{}{})
	if len(d.streak) != 0 {
		t.Fatalf("expected state to be pruned, got %d entries", len(d.streak))
	}
	if d.observe("container-a", true) {
		t.Fatalf("a pruned container must start its paused streak from scratch")
	}
}

// TestCountsAsUnhealthy: Docker reports "unhealthy" with FailingStreak 0 while a
// container is paused and for one interval after unpause; that must not count.
func TestCountsAsUnhealthy(t *testing.T) {
	tests := []struct {
		status        string
		failingStreak int
		want          bool
	}{
		{"unhealthy", 0, false}, // paused, or just unpaused: no probe has failed
		{"unhealthy", 1, true},
		{"unhealthy", 5, true},
		{"healthy", 0, false},
		{"starting", 2, false},
	}
	for _, tt := range tests {
		if got := countsAsUnhealthy(tt.status, tt.failingStreak); got != tt.want {
			t.Errorf("countsAsUnhealthy(%q, %d) = %v, want %v", tt.status, tt.failingStreak, got, tt.want)
		}
	}
}
