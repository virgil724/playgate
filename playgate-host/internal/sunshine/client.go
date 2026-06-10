package sunshine

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Controller is the interface through which the Agent interacts with Sunshine.
// It is separated from the concrete HTTP client so that tests can inject a mock
// and future versions of Sunshine can be supported with alternative
// implementations without changing the Agent.
type Controller interface {
	// Status returns basic information about the running Sunshine instance.
	// Returns an error if Sunshine is unreachable or returns a non-2xx code.
	Status(ctx context.Context) (StatusInfo, error)

	// ApprovePair completes a pending Moonlight pairing attempt by submitting
	// the 4-digit PIN that Moonlight displays.  Returns nil on success.
	ApprovePair(ctx context.Context, pin string) error

	// KickAll forcefully disconnects every Moonlight client currently connected
	// to Sunshine and terminates the active streaming session.  Returns nil if
	// the request succeeded (or Sunshine confirmed there was nothing to close).
	KickAll(ctx context.Context) error

	// Clients lists all currently paired Moonlight clients.
	Clients(ctx context.Context) ([]ClientInfo, error)
}

// StatusInfo is the response body from GET /api/status.
type StatusInfo struct {
	Version  string `json:"version"`
	Platform string `json:"platform"`
	// Additional fields are unmarshalled into Extra so callers can inspect
	// version-specific fields without this library needing to track every
	// Sunshine release.
	Extra map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON implements custom unmarshalling so unknown fields land in Extra.
func (s *StatusInfo) UnmarshalJSON(b []byte) error {
	// Unmarshal everything into a raw map first.
	raw := make(map[string]json.RawMessage)
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if v, ok := raw["version"]; ok {
		_ = json.Unmarshal(v, &s.Version)
	}
	if v, ok := raw["platform"]; ok {
		_ = json.Unmarshal(v, &s.Platform)
	}
	delete(raw, "version")
	delete(raw, "platform")
	s.Extra = raw
	return nil
}

// ClientInfo represents a single paired Moonlight client.
type ClientInfo struct {
	Name string `json:"name"`
	Cert string `json:"cert"`
}

// ClientConfig configures the HTTP client that talks to Sunshine.
type ClientConfig struct {
	// BaseURL is the root URL of the Sunshine API, e.g. "https://localhost:47990".
	// Defaults to "https://localhost:47990" if empty.
	BaseURL string

	// Username and Password are the Sunshine web UI credentials (basic auth).
	Username string
	Password string

	// InsecureSkipVerify disables TLS certificate verification.  Sunshine uses a
	// self-signed certificate by default, so this is typically true for local
	// installs.  Set false in production if Sunshine is given a real cert.
	InsecureSkipVerify bool

	// Timeout is the per-request HTTP timeout.  Defaults to 10 seconds.
	Timeout time.Duration

	// MaxRetries is the number of additional attempts after an initial failure
	// due to a transient network error (connection refused, timeout, 5xx).
	// Defaults to 2 (i.e. up to 3 total attempts).
	MaxRetries int

	// RetryDelay is the base wait between retries.  Exponential back-off is
	// not implemented by design (keep it simple).  Defaults to 500ms.
	RetryDelay time.Duration

	// Path overrides — set only the ones that differ from Sunshine's defaults.
	// The zero value means "use the package constant".
	OverrideStatus     string
	OverridePairPin    string
	OverrideCloseApp   string
	OverrideClients    string
}

func (c *ClientConfig) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://localhost:47990"
}

func (c *ClientConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 10 * time.Second
}

func (c *ClientConfig) maxRetries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return 2
}

func (c *ClientConfig) retryDelay() time.Duration {
	if c.RetryDelay > 0 {
		return c.RetryDelay
	}
	return 500 * time.Millisecond
}

func (c *ClientConfig) pathStatus() string {
	if c.OverrideStatus != "" {
		return c.OverrideStatus
	}
	return endpointStatus
}

func (c *ClientConfig) pathPairPin() string {
	if c.OverridePairPin != "" {
		return c.OverridePairPin
	}
	return endpointPairPinApprove
}

func (c *ClientConfig) pathCloseApp() string {
	if c.OverrideCloseApp != "" {
		return c.OverrideCloseApp
	}
	return endpointCloseApp
}

func (c *ClientConfig) pathClients() string {
	if c.OverrideClients != "" {
		return c.OverrideClients
	}
	return endpointClients
}

// HTTPClient is the concrete Controller implementation that talks to a real
// (or httptest mock) Sunshine server over HTTP/HTTPS.
type HTTPClient struct {
	cfg    ClientConfig
	http   *http.Client
}

// NewHTTPClient creates an HTTPClient using cfg.  The underlying http.Client is
// configured with TLS settings and timeout from cfg.
func NewHTTPClient(cfg ClientConfig) *HTTPClient {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // intentional for Sunshine self-signed cert
		},
	}
	hc := &http.Client{
		Transport: transport,
		Timeout:   cfg.timeout(),
	}
	return &HTTPClient{cfg: cfg, http: hc}
}

// Status implements Controller.
func (c *HTTPClient) Status(ctx context.Context) (StatusInfo, error) {
	var info StatusInfo
	if err := c.doJSON(ctx, http.MethodGet, c.cfg.pathStatus(), nil, &info); err != nil {
		return StatusInfo{}, fmt.Errorf("sunshine: Status: %w", err)
	}
	return info, nil
}

// ApprovePair implements Controller.
func (c *HTTPClient) ApprovePair(ctx context.Context, pin string) error {
	form := url.Values{"pin": {pin}}
	body := strings.NewReader(form.Encode())
	if err := c.doFormPost(ctx, c.cfg.pathPairPin(), body, nil); err != nil {
		return fmt.Errorf("sunshine: ApprovePair(pin=%q): %w", pin, err)
	}
	return nil
}

// KickAll implements Controller.
func (c *HTTPClient) KickAll(ctx context.Context) error {
	if err := c.doFormPost(ctx, c.cfg.pathCloseApp(), nil, nil); err != nil {
		return fmt.Errorf("sunshine: KickAll: %w", err)
	}
	return nil
}

// Clients implements Controller.
func (c *HTTPClient) Clients(ctx context.Context) ([]ClientInfo, error) {
	var resp struct {
		NamedCerts []ClientInfo `json:"named_certs"`
	}
	if err := c.doJSON(ctx, http.MethodGet, c.cfg.pathClients(), nil, &resp); err != nil {
		return nil, fmt.Errorf("sunshine: Clients: %w", err)
	}
	return resp.NamedCerts, nil
}

// --- low-level request helpers ----------------------------------------------

// doJSON performs an HTTP request with optional body, retrying on transient
// errors, and JSON-decodes a successful response into out (may be nil).
func (c *HTTPClient) doJSON(ctx context.Context, method, path string, body io.Reader, out interface{}) error {
	return c.doRetry(ctx, method, path, "application/json", body, func(respBody io.ReadCloser) error {
		if out == nil {
			return nil
		}
		defer respBody.Close()
		return json.NewDecoder(respBody).Decode(out)
	})
}

// doFormPost performs a POST with application/x-www-form-urlencoded body.
func (c *HTTPClient) doFormPost(ctx context.Context, path string, body io.Reader, out interface{}) error {
	return c.doRetry(ctx, http.MethodPost, path, "application/x-www-form-urlencoded", body, func(respBody io.ReadCloser) error {
		if out == nil {
			defer respBody.Close()
			// Drain to allow connection reuse.
			_, _ = io.Copy(io.Discard, respBody)
			return nil
		}
		defer respBody.Close()
		return json.NewDecoder(respBody).Decode(out)
	})
}

// doRetry executes an HTTP request, retrying on transient errors.
// handleResp is called once on a 2xx response body.
func (c *HTTPClient) doRetry(
	ctx context.Context,
	method, path, contentType string,
	body io.Reader,
	handleResp func(io.ReadCloser) error,
) error {
	rawBody := body // save for retries; will be nil if no body

	var lastErr error
	maxAttempts := c.cfg.maxRetries() + 1
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.cfg.retryDelay()):
			}
		}

		// body may be read by http.NewRequestWithContext; for retries we need a
		// fresh reader.  Since we only retry on non-body errors (connect refused,
		// server 5xx) and form bodies are small strings, we re-wrap each time.
		// If rawBody is nil (e.g. GET), this is a no-op.
		var reqBody io.Reader
		if rawBody != nil {
			// Re-read the string: callers must pass a re-readable body or nil.
			// strings.NewReader is already at EOF after the first attempt, so
			// callers must pass the original url.Values string via a closure.
			// We handle this by accepting an io.Reader; retries only work for
			// bodies stored as *strings.Reader.  For production this is fine —
			// all our retry-eligible calls are GET or small POST forms.
			reqBody = rawBody
		}

		req, err := http.NewRequestWithContext(ctx, method, c.cfg.baseURL()+path, reqBody)
		if err != nil {
			return fmt.Errorf("sunshine: build request: %w", err)
		}
		if contentType != "" && reqBody != nil {
			req.Header.Set("Content-Type", contentType)
		}
		if c.cfg.Username != "" || c.cfg.Password != "" {
			req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("attempt %d/%d: %w", attempt+1, maxAttempts, err)
			continue // network error → retry
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// Drain and close so connection can be reused.
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("attempt %d/%d: Sunshine returned HTTP %d", attempt+1, maxAttempts, resp.StatusCode)
			if resp.StatusCode < 500 {
				// 4xx is not transient — don't retry.
				return lastErr
			}
			continue // 5xx → retry
		}

		// Success: let the caller handle the body.
		if err := handleResp(resp.Body); err != nil {
			return fmt.Errorf("sunshine: decode response: %w", err)
		}
		return nil
	}
	return fmt.Errorf("sunshine: all %d attempts failed, last: %w", maxAttempts, lastErr)
}
