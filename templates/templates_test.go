package templates_test

import (
	"testing"

	"anz-training/templates"
)

func TestTemplates_AreEmbedded(t *testing.T) {
	if templates.DeployWorkflow == "" {
		t.Error("DeployWorkflow template is empty")
	}
	if templates.ServiceYAML == "" {
		t.Error("ServiceYAML template is empty")
	}
}
