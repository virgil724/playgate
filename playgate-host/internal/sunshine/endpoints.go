package sunshine

// Sunshine REST API endpoint paths.
//
// All paths are relative to the base URL (default https://localhost:47990).
// Sunshine does not publish a stable machine-readable API contract; these
// values were derived from the Sunshine v0.23.x source code and community
// documentation.
//
// NOTE: Depending on Sunshine version these paths may need adjustment.
// The Client accepts path overrides via ClientConfig.Override* fields so that
// operators can correct mismatches without recompiling.
const (
	// endpointStatus returns basic information about the running Sunshine
	// instance (version, platform, etc.).
	//
	// Method: GET
	// Response: JSON {"version":"0.23.0","platform":"windows",...}
	//
	// May need adjustment: confirmed on v0.23.x; older builds may not exist.
	endpointStatus = "/api/status"

	// endpointClients lists currently paired Moonlight clients.
	//
	// Method: GET
	// Response: JSON {"named_certs":[{"name":"...","cert":"..."},...]}
	endpointClients = "/api/clients"

	// endpointAppsList lists available Sunshine "apps" (streaming targets).
	//
	// Method: GET
	// Response: JSON {"apps":[{"title":"Desktop","index":"0"},...]}
	endpointAppsList = "/api/apps"

	// endpointPairPinApprove submits the PIN displayed by Moonlight to complete
	// device pairing.
	//
	// Method: POST  Content-Type: application/x-www-form-urlencoded
	// Body:   pin=<4-digit PIN>
	// Response: JSON {"status":"true"} on success
	//
	// Note: older Sunshine builds (< v0.19) used /pin instead.
	endpointPairPinApprove = "/api/pin"

	// endpointCloseApp forcefully terminates the currently streaming application
	// and disconnects connected Moonlight clients.
	//
	// Method: POST (no body required)
	// Response: JSON {"status":"true"} on success
	//
	// This is the primary kick mechanism.  Sunshine will terminate the RTSP/
	// video session; Moonlight will show a disconnection error.
	//
	// May need adjustment: confirmed on v0.23.x.
	endpointCloseApp = "/api/apps/close"

	// endpointQuit asks Sunshine to shut down gracefully.  Not used in normal
	// operation; exposed for completeness.
	//
	// Method: POST
	endpointQuit = "/api/quit"
)
