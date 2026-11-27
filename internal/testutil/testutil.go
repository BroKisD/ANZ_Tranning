// Package testutil holds small fakes and fixtures shared by tests in several packages.
package testutil

import (
	"bytes"
	"path/filepath"
	"runtime"

	"anz-training/model"
)

// TestKey returns a deterministic 32-byte AES key.
func TestKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// GCPCreds is a fully populated GCP credential set for tests.
var GCPCreds = model.GCPCreds{
	ProjectID: "my-proj", Region: "asia-southeast1", ArtifactRepo: "ai-apps",
	WIFProvider:    "projects/1/locations/global/workloadIdentityPools/p/providers/gh",
	ServiceAccount: "deployer@my-proj.iam.gserviceaccount.com",
}

// SchemaPath returns the absolute path of testdata/schema.sql (used by the Docker-backed integration tests).
func SchemaPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata", "schema.sql")
}
