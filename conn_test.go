package main

import (
	"context"
	"errors"
	"testing"

	"github.com/moby/moby/client"
)

func TestDaemonConnTracker(t *testing.T) {
	// A real connection-failed error can't be constructed outside the client
	// package, so build one via a client pointed at a missing socket.
	connErr := listErrFromMissingSocket(t)
	other := errors.New("boom")

	tests := []struct {
		name string
		errs []error
		exit []bool
	}{
		{"exits on third consecutive connection failure", []error{connErr, connErr, connErr, connErr}, []bool{false, false, true, true}},
		{"a single transient failure does not exit", []error{connErr, nil, connErr, nil}, []bool{false, false, false, false}},
		{"success resets the streak", []error{connErr, connErr, nil, connErr, connErr}, []bool{false, false, false, false, false}},
		{"non-connection errors never count", []error{other, other, other, context.DeadlineExceeded}, []bool{false, false, false, false}},
		{"a non-connection error resets the streak", []error{connErr, connErr, other, connErr}, []bool{false, false, false, false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &daemonConnTracker{}
			for i, err := range tc.errs {
				if got := tr.observe(err); got != tc.exit[i] {
					t.Errorf("poll %d: got %v, want %v", i, got, tc.exit[i])
				}
			}
		})
	}
}

func listErrFromMissingSocket(t *testing.T) error {
	t.Helper()
	c, err := client.NewClientWithOpts(client.WithHost("unix:///nonexistent/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.ContainerList(context.Background(), client.ContainerListOptions{})
	if !client.IsErrConnectionFailed(err) {
		t.Fatalf("expected connection-failed error, got %v", err)
	}
	return err
}
