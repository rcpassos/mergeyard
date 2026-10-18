// Package runtimeauth authenticates the CLI to the runtime across the loopback
// port, which every other local user can also reach.
//
// The runtime stores a random secret in the workspace with owner-only
// permissions. The CLI reads the same file and proves knowledge of it on each
// API request; the runtime proves itself in every response. The secret itself
// never travels over the port, so a process that merely binds the configured
// port cannot impersonate the runtime or learn the secret.
package runtimeauth
