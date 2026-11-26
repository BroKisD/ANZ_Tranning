package helper_test

import (
	"strings"
	"testing"

	"anz-training/helper"
)

func TestWebhookSecretFor(t *testing.T) {
	a, b := helper.WebhookSecretFor("root", "p1"), helper.WebhookSecretFor("root", "p2")
	if a == "" || a == b || a != helper.WebhookSecretFor("root", "p1") {
		t.Fatal("secret must be deterministic and per-project")
	}
	if strings.Contains(a, "root") || helper.WebhookSecretFor("other", "p1") == a {
		t.Fatal("secret must depend on the root secret and not reveal it")
	}
	if helper.WebhookSecretFor("", "p1") != "" {
		t.Fatal("empty root secret must give an empty (rejected) secret")
	}
}
