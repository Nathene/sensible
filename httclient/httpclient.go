package httclient

import (
	"net"
	"net/http"
	"time"
)

const (
	defaultTimeout      = 15 * time.Second
	defaultIdleTimeout  = 30 * time.Second
	defaultRetryMax     = 3
	defaultRetryWaitMin = 1 * time.Second
	defaultRetryWaitMax = 15 * time.Second
	defaultMaxIdleConns = 5
	defaultTLSHandshake = 5 * time.Second
	defaultKeepAlive    = 10 * time.Second
)

// Client wraps the standard http.Client with sensible defaults and retry capability
type Client struct {
	*http.Client
	retryMax     int
	retryWaitMin time.Duration
	retryWaitMax time.Duration
}

// New creates a new HTTP client with sensible defaults
func New(opts ...Option) *Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   defaultTimeout,
			KeepAlive: defaultKeepAlive,
		}).DialContext,
		MaxIdleConns:          defaultMaxIdleConns,
		IdleConnTimeout:       defaultIdleTimeout,
		TLSHandshakeTimeout:   defaultTLSHandshake,
		ExpectContinueTimeout: 1 * time.Second,
	}

	c := &Client{
		Client: &http.Client{
			Transport: transport,
			Timeout:   defaultTimeout,
		},
		retryMax:     defaultRetryMax,
		retryWaitMin: defaultRetryWaitMin,
		retryWaitMax: defaultRetryWaitMax,
	}

	// Apply any custom options
	for _, opt := range opts {
		opt(c)
	}

	return c
}

// Do wraps http.Client's Do method with retry capability
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error

	wait := c.retryWaitMin

	for i := 0; i <= c.retryMax; i++ {
		resp, err = c.Client.Do(req)

		if err != nil {
			if i == c.retryMax {
				return nil, err
			}

			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(wait):
			}

			// Exponential backoff
			wait *= 2
			if wait > c.retryWaitMax {
				wait = c.retryWaitMax
			}
			continue
		}

		// Check if we should retry based on status code
		if shouldRetry(resp.StatusCode) && i < c.retryMax {
			resp.Body.Close()
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(wait):
			}
			wait *= 2
			if wait > c.retryWaitMax {
				wait = c.retryWaitMax
			}
			continue
		}

		return resp, nil
	}

	return resp, err
}

// shouldRetry returns true if the status code indicates a retriable error
func shouldRetry(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests ||
		statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusGatewayTimeout ||
		statusCode == http.StatusBadGateway
}

// Option allows customization of the client
type Option func(*Client)

// WithTimeout sets the client timeout
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		c.Timeout = timeout
	}
}

// WithRetryMax sets the maximum number of retries
func WithRetryMax(max int) Option {
	return func(c *Client) {
		c.retryMax = max
	}
}

// WithRetryWaitMin sets the minimum retry wait time
func WithRetryWaitMin(min time.Duration) Option {
	return func(c *Client) {
		c.retryWaitMin = min
	}
}

// WithRetryWaitMax sets the maximum retry wait time
func WithRetryWaitMax(max time.Duration) Option {
	return func(c *Client) {
		c.retryWaitMax = max
	}
}

// WithMaxIdleConns sets the maximum number of idle connections
func WithMaxIdleConns(max int) Option {
	return func(c *Client) {
		c.Transport.(*http.Transport).MaxIdleConns = max
	}
}
