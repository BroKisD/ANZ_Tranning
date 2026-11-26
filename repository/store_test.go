package repository_test

import (
	"testing"

	"anz-training/handler"
	"anz-training/repository"
	"anz-training/service"
)

// Query behaviour needs a real database and is covered by store_integration_test.go
// (go test -tags=integration). These checks need no Docker.

func TestNewStore(t *testing.T) {
	if repository.NewStore(nil) == nil {
		t.Fatal("NewStore returned nil")
	}
}

func TestStore_SatisfiesConsumerInterfaces(t *testing.T) {
	var s any = repository.NewStore(nil)
	if _, ok := s.(service.UserRepository); !ok {
		t.Error("Store must implement service.UserRepository")
	}
	if _, ok := s.(service.ProjectStore); !ok {
		t.Error("Store must implement service.ProjectStore")
	}
	if _, ok := s.(handler.WebhookStore); !ok {
		t.Error("Store must implement handler.WebhookStore")
	}
}
