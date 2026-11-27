package main

import (
	"crypto/tls"
	"errors"
	"strings"

	"go.temporal.io/sdk/client"
)

// temporalClientOptions builds the Temporal connection from the environment.
//
//	TEMPORAL_ADDRESS    host:port (default localhost:7233, the local dev server)
//	TEMPORAL_NAMESPACE  namespace (default: Temporal's "default")
//	TEMPORAL_API_KEY    Temporal Cloud API key; turns on TLS and API-key credentials
//
// With no API key the connection is plain gRPC, exactly as for a local or
// self-hosted dev server. The key is never included in an error message.
func temporalClientOptions(get func(string) string) (client.Options, error) {
	addr := strings.TrimSpace(get("TEMPORAL_ADDRESS"))
	ns := strings.TrimSpace(get("TEMPORAL_NAMESPACE"))
	key := strings.TrimSpace(get("TEMPORAL_API_KEY"))

	opts := client.Options{HostPort: addr, Namespace: ns}
	if opts.HostPort == "" && key == "" {
		opts.HostPort = "localhost:7233"
	}
	if key == "" {
		return opts, nil
	}
	if addr == "" {
		return client.Options{}, errors.New("TEMPORAL_API_KEY is set but TEMPORAL_ADDRESS is not (use your Temporal Cloud regional endpoint)")
	}
	if ns == "" {
		return client.Options{}, errors.New("TEMPORAL_API_KEY is set but TEMPORAL_NAMESPACE is not (use <namespace>.<account id>)")
	}
	opts.Credentials = client.NewAPIKeyStaticCredentials(key)
	opts.ConnectionOptions = client.ConnectionOptions{TLS: &tls.Config{MinVersion: tls.VersionTLS12}}
	return opts, nil
}
