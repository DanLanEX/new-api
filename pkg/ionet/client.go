package ionet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

const (
	DefaultEnterpriseBaseURL = "https://api.io.solutions/enterprise/v1/io-cloud/caas"
	DefaultBaseURL           = "https://api.io.solutions/v1/io-cloud/caas"
	DefaultTimeout           = 30 * time.Second
)

var proxyURLProvider func() string

// SetProxyURLProvider allows the application layer to provide a saved proxy URL
// without making the ionet package depend on global application settings.
//
// NOTE: The proxy URL is read at client creation time, not per-request.
// Each call to NewClient/NewEnterpriseClient creates a fresh http.Client with
// the proxy configured at that moment. If the proxy setting changes later,
// already-created clients will continue using the old proxy. This is acceptable
// because the controller layer creates a new client per HTTP request.
func SetProxyURLProvider(provider func() string) {
	proxyURLProvider = provider
}

func getProvidedProxyURL() string {
	if proxyURLProvider == nil {
		return ""
	}
	return strings.TrimSpace(proxyURLProvider())
}

// DefaultHTTPClient is the default HTTP client implementation
type DefaultHTTPClient struct {
	client *http.Client
}

func newProxyTransport(proxyURL string) (*http.Transport, error) {
	parsedURL, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil {
		return nil, err
	}

	switch parsedURL.Scheme {
	case "http", "https":
		return &http.Transport{
			Proxy:             http.ProxyURL(parsedURL),
			ForceAttemptHTTP2: true,
		}, nil

	case "socks5", "socks5h":
		var auth *proxy.Auth
		if parsedURL.User != nil {
			auth = &proxy.Auth{
				User:     parsedURL.User.Username(),
				Password: "",
			}
			if password, ok := parsedURL.User.Password(); ok {
				auth.Password = password
			}
		}

		dialer, err := proxy.SOCKS5("tcp", parsedURL.Host, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}

		return &http.Transport{
			ForceAttemptHTTP2: true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		}, nil

	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s, must be http, https, socks5 or socks5h", parsedURL.Scheme)
	}
}

func newHTTPClient(timeout time.Duration, proxyURL string) (*http.Client, error) {
	client := &http.Client{
		Timeout: timeout,
	}

	if strings.TrimSpace(proxyURL) != "" {
		transport, err := newProxyTransport(proxyURL)
		if err != nil {
			return nil, err
		}
		client.Transport = transport
	}

	return client, nil
}

// NewDefaultHTTPClient creates a new default HTTP client.
// If the configured proxy URL is invalid, it falls back to a direct connection
// and logs a warning — proxy is optional, so a bad proxy should not break io.net.
func NewDefaultHTTPClient(timeout time.Duration) *DefaultHTTPClient {
	return newDefaultHTTPClientWithProxyURL(timeout, getProvidedProxyURL())
}

// newDefaultHTTPClientWithProxyURL creates a DefaultHTTPClient using the given
// proxy URL. On invalid proxy, falls back to direct connection with a warning.
func newDefaultHTTPClientWithProxyURL(timeout time.Duration, proxyURL string) *DefaultHTTPClient {
	client, err := newHTTPClient(timeout, proxyURL)
	if err != nil {
		// Proxy is optional — log warning and fall back to direct connection
		log.Printf("[ionet] WARNING: invalid proxy URL %q, falling back to direct connection: %v", proxyURL, err)
		return &DefaultHTTPClient{client: &http.Client{Timeout: timeout}}
	}
	return &DefaultHTTPClient{client: client}
}

// Do executes an HTTP request
func (c *DefaultHTTPClient) Do(req *HTTPRequest) (*HTTPResponse, error) {
	httpReq, err := http.NewRequest(req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers
	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	var body bytes.Buffer
	_, err = body.ReadFrom(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Convert headers
	headers := make(map[string]string)
	for key, values := range resp.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	return &HTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    headers,
		Body:       body.Bytes(),
	}, nil
}

// NewEnterpriseClient creates a new IO.NET API client targeting the enterprise API base URL.
// Uses the proxy URL from the registered provider; on invalid proxy, falls back to direct connection.
func NewEnterpriseClient(apiKey string) *Client {
	return NewClientWithConfig(apiKey, DefaultEnterpriseBaseURL, newDefaultHTTPClientWithProxyURL(DefaultTimeout, getProvidedProxyURL()))
}

// NewClient creates a new IO.NET API client targeting the public API base URL.
// Uses the proxy URL from the registered provider; on invalid proxy, falls back to direct connection.
func NewClient(apiKey string) *Client {
	return NewClientWithConfig(apiKey, DefaultBaseURL, newDefaultHTTPClientWithProxyURL(DefaultTimeout, getProvidedProxyURL()))
}

// NewClientWithConfig creates a new IO.NET API client with custom configuration
func NewClientWithConfig(apiKey, baseURL string, httpClient HTTPClient) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = NewDefaultHTTPClient(DefaultTimeout)
	}
	return &Client{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		HTTPClient: httpClient,
	}
}

// makeRequest performs an HTTP request and handles common response processing
func (c *Client) makeRequest(method, endpoint string, body interface{}) (*HTTPResponse, error) {
	var reqBody []byte
	var err error

	if body != nil {
		reqBody, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
	}

	headers := map[string]string{
		"X-API-KEY":    c.APIKey,
		"Content-Type": "application/json",
	}

	req := &HTTPRequest{
		Method:  method,
		URL:     c.BaseURL + endpoint,
		Headers: headers,
		Body:    reqBody,
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	// Handle API errors
	if resp.StatusCode >= 400 {
		var apiErr APIError
		if len(resp.Body) > 0 {
			// Try to parse the actual error format: {"detail": "message"}
			var errorResp struct {
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(resp.Body, &errorResp); err == nil && errorResp.Detail != "" {
				apiErr = APIError{
					Code:    resp.StatusCode,
					Message: errorResp.Detail,
				}
			} else {
				// Fallback: use raw body as details
				apiErr = APIError{
					Code:    resp.StatusCode,
					Message: fmt.Sprintf("API request failed with status %d", resp.StatusCode),
					Details: string(resp.Body),
				}
			}
		} else {
			apiErr = APIError{
				Code:    resp.StatusCode,
				Message: fmt.Sprintf("API request failed with status %d", resp.StatusCode),
			}
		}
		return nil, &apiErr
	}

	return resp, nil
}

// buildQueryParams builds query parameters for GET requests
func buildQueryParams(params map[string]interface{}) string {
	if len(params) == 0 {
		return ""
	}

	values := url.Values{}
	for key, value := range params {
		if value == nil {
			continue
		}
		switch v := value.(type) {
		case string:
			if v != "" {
				values.Add(key, v)
			}
		case int:
			if v != 0 {
				values.Add(key, strconv.Itoa(v))
			}
		case int64:
			if v != 0 {
				values.Add(key, strconv.FormatInt(v, 10))
			}
		case float64:
			if v != 0 {
				values.Add(key, strconv.FormatFloat(v, 'f', -1, 64))
			}
		case bool:
			values.Add(key, strconv.FormatBool(v))
		case time.Time:
			if !v.IsZero() {
				values.Add(key, v.Format(time.RFC3339))
			}
		case *time.Time:
			if v != nil && !v.IsZero() {
				values.Add(key, v.Format(time.RFC3339))
			}
		case []int:
			if len(v) > 0 {
				if encoded, err := json.Marshal(v); err == nil {
					values.Add(key, string(encoded))
				}
			}
		case []string:
			if len(v) > 0 {
				if encoded, err := json.Marshal(v); err == nil {
					values.Add(key, string(encoded))
				}
			}
		default:
			values.Add(key, fmt.Sprint(v))
		}
	}

	if len(values) > 0 {
		return "?" + values.Encode()
	}
	return ""
}
