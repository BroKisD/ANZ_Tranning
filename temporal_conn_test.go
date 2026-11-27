package main

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestTemporalClientOptions_LocalDefaults(t *testing.T) {
	o, err := temporalClientOptions(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if o.HostPort != "localhost:7233" || o.Namespace != "" || o.Credentials != nil || o.ConnectionOptions.TLS != nil {
		t.Fatalf("local defaults changed: %+v", o)
	}
}

func TestTemporalClientOptions_SelfHostedAddressAndNamespace(t *testing.T) {
	o, err := temporalClientOptions(env(map[string]string{"TEMPORAL_ADDRESS": "temporal:7233", "TEMPORAL_NAMESPACE": "training"}))
	if err != nil {
		t.Fatal(err)
	}
	if o.HostPort != "temporal:7233" || o.Namespace != "training" || o.Credentials != nil || o.ConnectionOptions.TLS != nil {
		t.Fatalf("options = %+v (no API key must mean no TLS and no credentials)", o)
	}
}

func TestTemporalClientOptions_TemporalCloudAPIKeyEnablesTLSAndCredentials(t *testing.T) {
	o, err := temporalClientOptions(env(map[string]string{
		"TEMPORAL_ADDRESS": "ap-southeast-1.aws.api.temporal.io:7233", "TEMPORAL_NAMESPACE": "my-ns.a1b2c", "TEMPORAL_API_KEY": "  key-value\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if o.HostPort != "ap-southeast-1.aws.api.temporal.io:7233" || o.Namespace != "my-ns.a1b2c" {
		t.Fatalf("options = %+v", o)
	}
	if o.Credentials == nil || o.ConnectionOptions.TLS == nil {
		t.Fatalf("an API key needs TLS and credentials: %+v", o)
	}
}

func TestTemporalClientOptions_APIKeyNeedsNamespaceAndAddress(t *testing.T) {
	for name, m := range map[string]map[string]string{
		"no namespace": {"TEMPORAL_ADDRESS": "x.api.temporal.io:7233", "TEMPORAL_API_KEY": "k"},
		"no address":   {"TEMPORAL_NAMESPACE": "ns.acct", "TEMPORAL_API_KEY": "k"},
	} {
		_, err := temporalClientOptions(env(m))
		if err == nil || !strings.Contains(err.Error(), "TEMPORAL_") {
			t.Errorf("%s: want an error naming the missing variable, got %v", name, err)
		}
	}
}

func TestTemporalClientOptions_ErrorNeverContainsTheKey(t *testing.T) {
	_, err := temporalClientOptions(env(map[string]string{"TEMPORAL_API_KEY": "super-secret-key"}))
	if err == nil || strings.Contains(err.Error(), "super-secret-key") {
		t.Fatalf("err = %v", err)
	}
}
