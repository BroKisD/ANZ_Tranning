// Package testutil holds small fakes and fixtures shared by tests in several packages.
package testutil

import (
	"bytes"

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
