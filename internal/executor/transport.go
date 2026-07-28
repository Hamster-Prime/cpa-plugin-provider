package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

type hostHTTPClientFactory func(proxyURL string) (pluginapi.HostHTTPClient, error)

const (
	defaultHTTPResponseLimit = int64(64 << 20)
	defaultNonStreamTimeout  = 5 * time.Minute
)

// ErrResponseBodyTooLarge reports that a non-streaming upstream response
// exceeded the configured in-memory safety limit.
var ErrResponseBodyTooLarge = errors.New("upstream response body is too large")

type safeHTTPClient struct {
	client          *http.Client
	transport       *http.Transport
	maxResponseBody int64
}

// NewHTTPClient builds the same proxy-aware, same-origin redirect transport
// used for provider execution. Management probes use it to match live traffic.
func NewHTTPClient(proxyURL string) (pluginapi.HostHTTPClient, error) {
	return newSafeHTTPClientWithLimit(proxyURL, defaultHTTPResponseLimit)
}

func newSafeHTTPClient(proxyURL string) (pluginapi.HostHTTPClient, error) {
	return newSafeHTTPClientWithLimit(proxyURL, defaultHTTPResponseLimit)
}

// NewHTTPClientWithResponseLimit builds a proxy-aware transport with a tighter
// non-streaming response limit for management operations such as model discovery.
func NewHTTPClientWithResponseLimit(proxyURL string, maxResponseBody int64) (pluginapi.HostHTTPClient, error) {
	return newSafeHTTPClientWithLimit(proxyURL, maxResponseBody)
}

func newSafeHTTPClientWithLimit(proxyURL string, maxResponseBody int64) (pluginapi.HostHTTPClient, error) {
	transport, _, err := proxyutil.BuildHTTPTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok && defaultTransport != nil {
			transport = defaultTransport.Clone()
		} else {
			transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
		}
	}
	if maxResponseBody <= 0 {
		maxResponseBody = defaultHTTPResponseLimit
	}
	return &safeHTTPClient{
		transport:       transport,
		maxResponseBody: maxResponseBody,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: sameOriginRedirect,
		},
	}, nil
}

func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) > 0 && !sameHTTPOrigin(via[0].URL, req.URL) {
		return http.ErrUseLastResponse
	}
	return nil
}

func sameHTTPOrigin(left, right *url.URL) bool {
	if left == nil || right == nil || !strings.EqualFold(left.Scheme, right.Scheme) || !strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	return effectivePort(left) == effectivePort(right)
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func (c *safeHTTPClient) Do(ctx context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, defaultNonStreamTimeout)
	defer cancel()
	httpRequest, errRequest := pluginHTTPRequest(requestCtx, request)
	if errRequest != nil {
		return pluginapi.HTTPResponse{}, errRequest
	}
	response, errDo := c.client.Do(httpRequest)
	if errDo != nil {
		c.transport.CloseIdleConnections()
		return pluginapi.HTTPResponse{}, errDo
	}
	defer c.transport.CloseIdleConnections()
	defer response.Body.Close()
	body, errRead := readLimitedResponseBody(response.Body, c.maxResponseBody)
	if errRead != nil {
		return pluginapi.HTTPResponse{}, errRead
	}
	return pluginapi.HTTPResponse{
		StatusCode: response.StatusCode,
		Headers:    response.Header.Clone(),
		Body:       body,
	}, nil
}

func readLimitedResponseBody(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = defaultHTTPResponseLimit
	}
	limited := &io.LimitedReader{R: body, N: limit + 1}
	payload, errRead := io.ReadAll(limited)
	if errRead != nil {
		return nil, errRead
	}
	if int64(len(payload)) > limit {
		return nil, fmt.Errorf("%w (limit %d bytes)", ErrResponseBodyTooLarge, limit)
	}
	return payload, nil
}

func (c *safeHTTPClient) DoStream(ctx context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	httpRequest, errRequest := pluginHTTPRequest(ctx, request)
	if errRequest != nil {
		return pluginapi.HTTPStreamResponse{}, errRequest
	}
	response, errDo := c.client.Do(httpRequest)
	if errDo != nil {
		c.transport.CloseIdleConnections()
		return pluginapi.HTTPStreamResponse{}, errDo
	}
	chunks := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(chunks)
		defer c.transport.CloseIdleConnections()
		defer response.Body.Close()
		buffer := make([]byte, 32*1024)
		for {
			read, errRead := response.Body.Read(buffer)
			if read > 0 {
				payload := append([]byte(nil), buffer[:read]...)
				select {
				case chunks <- pluginapi.HTTPStreamChunk{Payload: payload}:
				case <-ctx.Done():
					return
				}
			}
			if errRead != nil {
				if !errors.Is(errRead, io.EOF) {
					select {
					case chunks <- pluginapi.HTTPStreamChunk{Err: errRead}:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{
		StatusCode: response.StatusCode,
		Headers:    response.Header.Clone(),
		Chunks:     chunks,
	}, nil
}

func pluginHTTPRequest(ctx context.Context, request pluginapi.HTTPRequest) (*http.Request, error) {
	method := strings.TrimSpace(request.Method)
	if method == "" {
		method = http.MethodGet
	}
	httpRequest, err := http.NewRequestWithContext(ctx, method, request.URL, bytes.NewReader(request.Body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header = request.Headers.Clone()
	return httpRequest, nil
}
