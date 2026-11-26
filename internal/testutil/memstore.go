package testutil

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"anz-training/crypto"
	"anz-training/model"
)

type MemStore struct {
	mu       sync.Mutex
	Projects map[string]*model.Project
	history  []string // "status|step" after every write
}

func (m *MemStore) Add(p model.Project) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := p
	m.Projects[p.ID] = &cp
}

func (m *MemStore) apply(p *model.Project, f map[string]any) {
	for k, v := range f {
		s, _ := v.(string)
		switch k {
		case "status":
			p.Status = s
		case "current_step":
			p.CurrentStep = s
		case "repo_url":
			p.RepoURL = s
		case "pr_url":
			p.PRURL = s
		case "live_url":
			p.LiveURL = s
		case "error_message":
			p.ErrorMessage = s
		}
	}
	m.history = append(m.history, p.Status+"|"+p.CurrentStep)
}

func (m *MemStore) GetProject(_ context.Context, id string) (*model.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.Projects[id]
	if !ok {
		return nil, model.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *MemStore) UpdateProject(_ context.Context, id string, f map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.Projects[id]
	if !ok {
		return model.ErrNotFound
	}
	m.apply(p, f)
	return nil
}

func (m *MemStore) UpdateProjectIf(_ context.Context, id, from string, f map[string]any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.Projects[id]
	if !ok || p.Status != from {
		return false, nil
	}
	m.apply(p, f)
	return true, nil
}

func (m *MemStore) CreateProject(_ context.Context, p *model.Project) error {
	m.Add(*p)
	return nil
}

func (m *MemStore) Get(id string) model.Project {
	p, _ := m.GetProject(context.Background(), id)
	return *p
}

func (m *MemStore) Hist() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.history...)
}

type MemIntegrations struct {
	mu   sync.Mutex
	Rows map[string]model.UserIntegration
}

func (m *MemIntegrations) GetIntegration(_ context.Context, u, p string) (*model.UserIntegration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.Rows[u+"|"+p]
	if !ok {
		return nil, model.ErrNotFound
	}
	return &r, nil
}

func (m *MemIntegrations) Put(t *testing.T, key []byte, user, provider string, creds any) {
	t.Helper()
	plain, _ := json.Marshal(creds)
	enc, err := crypto.Encrypt(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.Rows[user+"|"+provider] = model.UserIntegration{UserID: user, Provider: provider, EncryptedCredentials: enc}
	m.mu.Unlock()
}
func NewMemStore() *MemStore { return &MemStore{Projects: map[string]*model.Project{}} }

func NewMemIntegrations() *MemIntegrations {
	return &MemIntegrations{Rows: map[string]model.UserIntegration{}}
}
