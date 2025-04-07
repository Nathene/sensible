package httpclient

import (
	"io"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
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

// BackoffStrategy defines the type of backoff strategy to use
// similar to how the net/http handles status codes.
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
// and if provided with any Option functions, applies them
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
// It attempts to send an HTTP request and retries based on the status code
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	tracer := otel.Tracer("httpclient")
	ctx, span := tracer.Start(req.Context(), "httpclient.Do")
	defer span.End()
	req = req.WithContext(ctx)

	wait := c.retryWaitMin

	for i := range c.retryMax {
		resp, err := c.Client.Do(req)

		_, ok := c.backoffStrategy[resp.StatusCode]
		if err == nil && !ok {
			break
		}
		if err != nil {
			if i == c.retryMax {
				return nil, err
			}

			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(wait):
			}

			wait = c.getNextWait(resp.StatusCode, wait)
			continue
		}

		if ok {
			switch c.backoffStrategy[resp.StatusCode] {
			case ConstantBackoff:
				wait = c.retryWaitMin
			case ExponentialBackoff:
				wait *= 2
				if wait > c.retryWaitMax {
					wait = c.retryWaitMax
				}
			}
		}
	}
	return c.Client.Do(req)
}

// Option allows customization of the Client
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

// WithBackoffStrategy sets the backoff strategy for a specific status code
func WithBackoffStrategy(statusCode int, strategy BackoffStrategy) Option {
	return func(c *Client) {
		if c.backoffStrategy == nil {
			c.backoffStrategy = make(map[int]BackoffStrategy)
		}
		c.backoffStrategy[statusCode] = strategy
	}
}

// getNextWait calculates the next wait time based on the current wait time and status code
// It uses the configured backoff strategy for the status code
// If no strategy is configured, it defaults to constant backoff
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
// This method takes an io.ReadCloser (response body)
// and returns a new io.ReadCloser with a limited reader to limit the size of the response payload.
// This is useful to prevent reading large response payloads into memory.
// This can also be configured via WithMaxResponseSize option.
func (c *Client) LimitedReader(body io.ReadCloser) io.ReadCloser {
	return &limitedReadCloser{
		io.LimitReader(body, c.maxResponseSize),
		body,
	}
}

// limitedReadCloser is a wrapper around an io.Reader and io.Closer
// It ensures that the response body is limited to a certain size and closed after reading.
type limitedReadCloser struct {
	io.Reader // the limited reader that restricts the amount of data that can be read
	io.Closer // the original response body that will be closed after reading
}

// Read reads data into p from the limited reader
// It implements the Read method of the io.Reader Interface
func (l *limitedReadCloser) Read(p []byte) (n int, err error) {
	if lr, ok := l.Reader.(*io.LimitedReader); ok {
		return lr.Read(p)
	}
	return 0, io.ErrUnexpectedEOF
}

// Close closes the original io.Closer
// It implements the Close method of the io.Closer Interface
func (l *limitedReadCloser) Close() error {
	return l.Closer.Close()
}
