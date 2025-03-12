package httpclient

import (
	"io"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	defaultTimeout         = 15 * time.Second
	defaultIdleTimeout     = 30 * time.Second
	defaultRetryMax        = 3
	defaultRetryWaitMin    = 1 * time.Second
	defaultRetryWaitMax    = 15 * time.Second
	defaultMaxIdleConns    = 5
	defaultTLSHandshake    = 5 * time.Second
	defaultKeepAlive       = 10 * time.Second
	defaultMaxResponseSize = 1024 * 1024 // 1MB default max response size
)

type BackoffStrategy int

const (
	ConstantBackoff BackoffStrategy = iota
	ExponentialBackoff
)

// Client wraps the standard http.Client with sensible defaults and retry capability.
// It provides:
// - Configurable retry behavior with exponential backoff for rate limits
// - Constant backoff for server errors
// - Context cancellation support
// - Connection pooling with sensible defaults
type Client struct {
	*http.Client
	retryMax        int           // Maximum number of retry attempts
	retryWaitMin    time.Duration // Minimum time to wait between retries
	retryWaitMax    time.Duration // Maximum time to wait between retries
	maxResponseSize int64         // Maximum response size in bytes
	backoffStrategy map[int]BackoffStrategy
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

	// Wrap the transport with OTEL
	tracedTransport := otelhttp.NewTransport(transport)

	c := &Client{
		Client: &http.Client{
			Transport: tracedTransport,
			Timeout:   defaultTimeout,
		},
		retryMax:        defaultRetryMax,
		retryWaitMin:    defaultRetryWaitMin,
		retryWaitMax:    defaultRetryWaitMax,
		maxResponseSize: defaultMaxResponseSize,
		backoffStrategy: map[int]BackoffStrategy{
			http.StatusTooManyRequests:    ExponentialBackoff,
			http.StatusServiceUnavailable: ConstantBackoff,
			http.StatusGatewayTimeout:     ConstantBackoff,
			http.StatusBadGateway:         ConstantBackoff,
		},
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

			wait = c.getNextWait(0, wait)
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
			wait = c.getNextWait(resp.StatusCode, wait)
			continue
		}

		// Wrap the response body with a limited reader
		resp.Body = c.LimitedReader(resp.Body)

		return resp, nil
	}

	return resp, err
}

// shouldRetry returns true if the status code indicates a retriable error
func shouldRetry(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests: // 429 - Use exponential backoff
		return true
	case http.StatusServiceUnavailable: // 503
		return true
	case http.StatusGatewayTimeout: // 504
		return true
	case http.StatusBadGateway: // 502
		return true
	default:
		return false
	}
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
		if t, ok := c.Transport.(*http.Transport); ok {
			t.MaxIdleConns = max
		}
	}
}

// WithMaxResponseSize sets maximum response size
func WithMaxResponseSize(maxBytes int64) Option {
	return func(c *Client) {
		c.maxResponseSize = maxBytes
	}
}

func WithBackoffStrategy(statusCode int, strategy BackoffStrategy) Option {
	return func(c *Client) {
		if c.backoffStrategy == nil {
			c.backoffStrategy = make(map[int]BackoffStrategy)
		}
		c.backoffStrategy[statusCode] = strategy
	}
}

func (c *Client) getNextWait(statusCode int, currentWait time.Duration) time.Duration {
	// Get configured strategy for this status code
	strategy, exists := c.backoffStrategy[statusCode]
	if !exists {
		// Default to constant backoff if no strategy configured
		return c.retryWaitMin
	}

	switch strategy {
	case ExponentialBackoff:
		wait := currentWait * 2
		if wait > c.retryWaitMax {
			return c.retryWaitMax
		}
		return wait
	case ConstantBackoff:
		return c.retryWaitMin
	default:
		return c.retryWaitMin
	}
}

// LimitedReader wraps response body with size limit
func (c *Client) LimitedReader(body io.ReadCloser) io.ReadCloser {
	return &limitedReadCloser{
		R: io.LimitReader(body, c.maxResponseSize),
		C: body,
	}
}

type limitedReadCloser struct {
	R io.Reader
	C io.Closer
}

func (l *limitedReadCloser) Read(p []byte) (n int, err error) {
	return l.R.Read(p)
}

func (l *limitedReadCloser) Close() error {
	return l.C.Close()
}
