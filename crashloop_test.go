package main

import "testing"

// TestCrashLoopDetector covers the state machine at the centre of #108: a
// genuine restart loop must be flagged, while a fresh container (deploy) or
// a single crash-and-recover must not be.
func TestCrashLoopDetector(t *testing.T) {
	tests := []struct {
		name    string
		polls   []int // RestartCount observed on each successive poll
		flagged []bool
	}{
		{
			name:    "genuine crash loop flags on the second consecutive rise",
			polls:   []int{5, 6, 7, 8},
			flagged: []bool{false, false, true, true},
		},
		{
			name:    "new container from a deploy is not flagged",
			polls:   []int{0, 0, 0},
			flagged: []bool{false, false, false},
		},
		{
			name:    "single crash-and-recover is not flagged",
			polls:   []int{2, 3, 3, 3},
			flagged: []bool{false, false, false, false},
		},
		{
			name:    "a rise followed by a plateau resets the streak",
			polls:   []int{1, 2, 2, 3, 4},
			flagged: []bool{false, false, false, false, true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newCrashLoopDetector()
			for i, count := range tt.polls {
				got := d.observe("container-a", count)
				if got != tt.flagged[i] {
					t.Errorf("poll %d (RestartCount=%d): got flagged=%v, want %v", i, count, got, tt.flagged[i])
				}
			}
		})
	}
}

// TestCrashLoopDetectorPrune ensures state for a container that disappears
// (e.g. replaced by a deploy) doesn't leak into a same-ID reuse or grow
// the map unboundedly.
func TestCrashLoopDetectorPrune(t *testing.T) {
	d := newCrashLoopDetector()
	d.observe("container-a", 5)
	d.observe("container-a", 6)
	if len(d.state) != 1 {
		t.Fatalf("expected 1 tracked container, got %d", len(d.state))
	}

	d.prune(map[string]struct{}{}) // container-a no longer present
	if len(d.state) != 0 {
		t.Fatalf("expected state to be pruned, got %d entries", len(d.state))
	}

	// A new container reusing the same ID starts its own streak from scratch.
	if flagged := d.observe("container-a", 0); flagged {
		t.Fatalf("freshly-pruned container should not be flagged on first observation")
	}
}
