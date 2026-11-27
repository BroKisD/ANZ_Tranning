package handler_test

import (
	"anz-training/handler"
	"anz-training/model"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// MockUserService implements UserService for testing purposes
type MockUserService struct {
	CreateFunc     func(ctx context.Context, email, name string) (*model.User, error)
	GetByEmailFunc func(ctx context.Context, email string) (*model.User, error)
}

func (m *MockUserService) Create(ctx context.Context, email, name string) (*model.User, error) {
	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, email, name)
	}
	return nil, nil
}

func (m *MockUserService) GetByEmail(ctx context.Context, email string) (*model.User, error) {
	if m.GetByEmailFunc != nil {
		return m.GetByEmailFunc(ctx, email)
	}
	return nil, model.ErrNotFound
}

func TestHandler_Create(t *testing.T) {
	t.Run("Success - 201 Created", func(t *testing.T) {
		mockSvc := &MockUserService{
			CreateFunc: func(ctx context.Context, email, name string) (*model.User, error) {
				return &model.User{Email: email, Name: name}, nil
			},
		}

		handler := handler.NewUserHandler(mockSvc)

		reqBody := map[string]string{"email": "test@example.com", "name": "Test User"}
		body, _ := json.Marshal(reqBody)

		req := httptest.NewRequest(http.MethodPost, "/users", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		handler.Create(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("expected status %d, got %d", http.StatusCreated, rec.Code)
		}
	})

	t.Run("Failure - 409 Conflict (Email Taken)", func(t *testing.T) {
		mockSvc := &MockUserService{
			CreateFunc: func(ctx context.Context, email, name string) (*model.User, error) {
				return nil, model.ErrEmailTaken
			},
		}

		handler := handler.NewUserHandler(mockSvc)

		reqBody := map[string]string{"email": "existing@example.com", "name": "User"}
		body, _ := json.Marshal(reqBody)

		req := httptest.NewRequest(http.MethodPost, "/users", bytes.NewBuffer(body))
		rec := httptest.NewRecorder()

		handler.Create(rec, req)

		if rec.Code != http.StatusConflict {
			t.Errorf("expected status %d, got %d", http.StatusConflict, rec.Code)
		}
	})
}

func TestHandler_Get(t *testing.T) {
	t.Run("Success - 200 OK", func(t *testing.T) {
		mockSvc := &MockUserService{
			GetByEmailFunc: func(ctx context.Context, email string) (*model.User, error) {
				return &model.User{Email: email, Name: "Found User"}, nil
			},
		}

		handler := handler.NewUserHandler(mockSvc)

		req := httptest.NewRequest(http.MethodGet, "/users?email=test@example.com", nil)
		rec := httptest.NewRecorder()

		handler.Get(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rec.Code)
		}
	})

	t.Run("Failure - 404 Not Found", func(t *testing.T) {
		mockSvc := &MockUserService{
			GetByEmailFunc: func(ctx context.Context, email string) (*model.User, error) {
				return nil, model.ErrNotFound
			},
		}

		handler := handler.NewUserHandler(mockSvc)

		req := httptest.NewRequest(http.MethodGet, "/users?email=notfound@example.com", nil)
		rec := httptest.NewRecorder()

		handler.Get(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected status %d, got %d", http.StatusNotFound, rec.Code)
		}
	})
}
