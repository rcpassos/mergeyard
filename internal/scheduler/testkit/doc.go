// Package testkit supplies scheduler test boundaries and lifecycle helpers.
// NewScenario runs policy and journal tests with controlled sessions and Git
// outcomes. TmuxFlow supplies the real-process fixtures used by integration
// tests. Both use the application's actual SQLite store and workflow journals.
package testkit
