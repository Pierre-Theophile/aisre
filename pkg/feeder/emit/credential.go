// SPDX-License-Identifier: Apache-2.0

package emit

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// A feeder's credential is either a fixed token or a file it re-reads on every call. The file
// is how a feeder running as a pod uses a projected ServiceAccount token: the kubelet rewrites
// it before it expires, so a token read once at start-up would stop working within the hour,
// while a token read per call never goes stale.

// ErrInsecureTransport is returned when a credential would be sent over plaintext HTTP to an
// address other than loopback.
var ErrInsecureTransport = errors.New("refusing to send a bearer token over plaintext http to a non-loopback address")

// TokenSource returns the bearer token for one call.
type TokenSource func() (string, error)

// StaticToken returns the same token on every call.
func StaticToken(token string) TokenSource {
	return func() (string, error) { return token, nil }
}

// TokenFile re-reads path on every call, so a token the platform rotates on disk is always
// current. An empty file is an error, not an anonymous call.
func TokenFile(path string) TokenSource {
	return func() (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("token file: %w", err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", fmt.Errorf("token file %s is empty", path)
		}
		return token, nil
	}
}

// CheckTransport refuses a plaintext http base URL whose host is not loopback. A bearer token
// sent in clear is readable, and replayable until it expires, by anything on the path; the
// usual cause is a `--server host:port` written without a scheme, which reads as http.
// `localhost`, 127.0.0.0/8 and ::1 stay allowed, for development and tests.
func CheckTransport(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("emit: the graph's base URL is not a URL: %w", redactURLError(err))
	}
	if u.Scheme != "http" {
		return nil
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%w (%s): use https://, or --insecure-transport for a lab network you trust",
		ErrInsecureTransport, u.Redacted())
}
