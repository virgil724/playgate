// Package sunshine implements the PlayGate Sunshine agent (T15).
//
// # Overview
//
// In PC mode PlayGate does not touch video or controller input at all.
// Instead it acts as a management agent on top of Sunshine/Moonlight:
//
//   - Sunshine (https://app.lizardbyte.dev/Sunshine/) runs on the host PC,
//     encodes the screen, and handles Moonlight client connections.
//   - This agent monitors internal/session.Manager events to know when a
//     viewer has been granted control or has had their session expire/kicked.
//   - On grant it approves the pending Moonlight pairing request and starts
//     counting down via the session timer.
//   - On expiry/idle-kick it calls the Sunshine REST API to forcefully
//     disconnect every connected client (KickAll).
//
// # Sunshine REST API assumptions
//
// Sunshine does not ship a formal machine-readable API contract.  The endpoint
// table below is derived from the Sunshine source code and community docs.
// Paths are centralised in endpoints.go; they may need adjustment for newer
// Sunshine releases — the client is designed so that callers can override
// every path via ClientConfig.Override*.
//
// # Package layout
//
//   - client.go      — HTTP transport, auth, retries, Controller interface
//   - endpoints.go   — all endpoint path constants (adjust per Sunshine version)
//   - agent.go       — event-driven management loop (consumes session.Events)
//   - agent_test.go  — httptest-backed integration tests
package sunshine
