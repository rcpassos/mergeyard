// Package integration_test exercises scheduler behavior against real Git,
// native fixture executables, tmux sessions, and process death. It is included
// by CI's recursive package selection. Tests that transfer interactive leases
// stay serial because those leases intentionally survive in child processes.
package integration_test
