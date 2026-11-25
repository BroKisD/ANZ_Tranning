package helper

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// WebhookSecretFor derives the deploy-webhook secret of one project from the
// platform's root secret. The derived value is what ends up in that project's
// (user-readable) workflow file, so it can only authenticate callbacks for that
// single project; the root secret never leaves the server. An empty root yields
// an empty secret, which both the workflow renderer and the webhook reject.
func WebhookSecretFor(root, projectID string) string {
	if root == "" {
		return ""
	}
	m := hmac.New(sha256.New, []byte(root))
	m.Write([]byte("deploy-webhook:" + projectID))
	return hex.EncodeToString(m.Sum(nil))
}
