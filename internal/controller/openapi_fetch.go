/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/hedinit/azure-apim-operator/internal/apim"
)

const (
	// openAPIFetchTimeout bounds one attempt to download a tenant's OpenAPI document.
	openAPIFetchTimeout = 15 * time.Second
	// maxOpenAPIBytes caps the document size. APIM rejects imports far smaller than this.
	maxOpenAPIBytes int64 = 8 << 20
	// maxOpenAPIRedirects keeps a redirecting endpoint from walking the operator around the network.
	maxOpenAPIRedirects = 3
	// requeueFetchFailure is how long an APIMAPIDeployment waits after a failed fetch. The
	// wait happens in the workqueue, so one unreachable endpoint no longer blocks the worker.
	requeueFetchFailure = 30 * time.Second
)

var (
	errBlockedOpenAPIIP = errors.New("destination address is not allowed for OpenAPI fetches")

	// defaultOpenAPIFetcher is what the deployment controller uses unless a test injects one.
	defaultOpenAPIFetcher = newOpenAPIFetcher(openAPIFetchTimeout, maxOpenAPIBytes, false)
)

// openAPIFetcher downloads OpenAPI documents from URLs that tenants control. Every fetch is
// bounded by a timeout and a size cap, may only use http or https, follows at most a few
// redirects, and never connects to loopback, link-local (the Azure IMDS endpoint lives
// there), multicast or unspecified addresses. Private ranges stay reachable because the
// documents live on in-cluster Services.
type openAPIFetcher struct {
	client   *http.Client
	maxBytes int64
}

func newOpenAPIFetcher(timeout time.Duration, maxBytes int64, allowLoopback bool) *openAPIFetcher {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%w: %q", errBlockedOpenAPIIP, address)
			}
			if blockedDestination(net.ParseIP(host), allowLoopback) {
				return fmt.Errorf("%w: %s", errBlockedOpenAPIIP, host)
			}
			return nil
		},
	}
	transport := &http.Transport{
		// No proxy: a proxy would bypass the destination check above.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          8,
		IdleConnTimeout:       time.Minute,
	}
	return &openAPIFetcher{
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > maxOpenAPIRedirects {
					return fmt.Errorf("stopped after %d redirects", maxOpenAPIRedirects)
				}
				return checkOpenAPIURL(req.URL)
			},
		},
		maxBytes: maxBytes,
	}
}

// azureWireServer is the Azure host agent every Azure VM reaches. It is not link-local, so
// the checks below would let it through.
var azureWireServer = net.IPv4(168, 63, 129, 16)

// blockedDestination reports whether an OpenAPI fetch may not connect to ip. Cluster
// Services use private ranges, so those stay open; everything that would reach the node,
// the pod itself, the cloud metadata endpoint or the Azure host agent is refused.
func blockedDestination(ip net.IP, allowLoopback bool) bool {
	if ip == nil || ip.Equal(azureWireServer) {
		return true
	}
	if ip.IsLoopback() {
		return !allowLoopback
	}
	return ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast()
}

// checkOpenAPIURL accepts only plain http(s) URLs with a host and no credentials.
func checkOpenAPIURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("OpenAPI URL must use http or https, got %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("OpenAPI URL has no host")
	}
	if u.User != nil {
		return errors.New("OpenAPI URL must not carry credentials")
	}
	return nil
}

// Fetch downloads and sanity-checks the OpenAPI document at rawURL. Errors never include
// the response body, so a misdirected fetch leaks nothing into status or logs.
func (f *openAPIFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid OpenAPI URL: %w", err)
	}
	if err := checkOpenAPIURL(u); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build OpenAPI request: %w", err)
	}
	req.Header.Set("Accept", "application/json, application/yaml, text/yaml;q=0.9, */*;q=0.1")

	// The URL can carry a key in its query string; errors, which end up in the status,
	// name it without one. A *url.Error from Do repeats the full URL, so only its cause is kept.
	shown := apim.RedactURL(u.String())
	resp, err := f.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("GET %s: %w", shown, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: unexpected status %s", shown, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read OpenAPI document: %w", err)
	}
	if int64(len(body)) > f.maxBytes {
		return nil, fmt.Errorf("OpenAPI document exceeds %d bytes", f.maxBytes)
	}
	if err := apim.ValidateOpenAPIDocument(body); err != nil {
		return nil, fmt.Errorf("response is not a usable OpenAPI document: %w", err)
	}
	return body, nil
}
