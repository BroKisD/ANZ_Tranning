// Package handler contains the HTTP handlers for the AI app builder API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"anz-training/crypto"
	"anz-training/helper"
	"anz-training/model"
)

// IntegrationStore is the persistence the integration handlers need.
type IntegrationStore interface {
	UpsertIntegration(ctx context.Context, in *model.UserIntegration) error
	GetIntegration(ctx context.Context, userID, provider string) (*model.UserIntegration, error)
	ListIntegrations(ctx context.Context, userID string) ([]model.UserIntegration, error)
}

// SecretStore keeps BYOK secrets outside MySQL and returns a reference path.
type SecretStore interface {
	PutSecret(ctx context.Context, secretID, value string) (ref string, err error)
}

// OwnerResolver looks up the GitHub account that owns an App installation
// (used when the callback does not carry the owner).
type OwnerResolver func(ctx context.Context, installationID int64) (string, error)

// UserIDFunc identifies the caller. Real session auth plugs in here.
type UserIDFunc func(r *http.Request) (string, error)

// HeaderUserID reads the caller from X-User-ID. Only safe behind the API-key
// middleware until real user auth exists.
func HeaderUserID(r *http.Request) (string, error) {
	id := strings.TrimSpace(r.Header.Get("X-User-ID"))
	if id == "" {
		return "", errors.New("missing user")
	}
	return id, nil
}

type IntegrationHandler struct {
	store   IntegrationStore
	secrets SecretStore // may be nil: BYOK then rejected
	key     []byte
	userID  UserIDFunc
	owner   OwnerResolver // may be nil: owner must be supplied
}

func NewIntegrationHandler(store IntegrationStore, secrets SecretStore, masterKey []byte, userID UserIDFunc, owner OwnerResolver) *IntegrationHandler {
	return &IntegrationHandler{store: store, secrets: secrets, key: masterKey, userID: userID, owner: owner}
}

func (h *IntegrationHandler) save(ctx context.Context, userID, provider string, creds any) error {
	plain, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	enc, err := crypto.Encrypt(h.key, plain)
	if err != nil {
		return err
	}
	return h.store.UpsertIntegration(ctx, &model.UserIntegration{
		ID: helper.NewID(), UserID: userID, Provider: provider, EncryptedCredentials: enc,
	})
}

// GitHubCallback handles GET /api/integrations/github/callback?installation_id=..[&owner=..]
// and saves installation_id & owner (provider = 'github'). No token is stored.
func (h *IntegrationHandler) GitHubCallback(w http.ResponseWriter, r *http.Request) {
	userID, err := h.userID(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	instID, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if err != nil || instID <= 0 {
		writeErr(w, http.StatusBadRequest, "valid installation_id required")
		return
	}
	owner := strings.TrimSpace(r.URL.Query().Get("owner"))
	if h.owner != nil { // GitHub is authoritative; ignore a client-supplied owner
		if owner, err = h.owner(r.Context(), instID); err != nil {
			log.Printf("resolve github owner: %v", err)
			writeErr(w, http.StatusBadGateway, "could not resolve github owner")
			return
		}
	}
	if owner == "" {
		writeErr(w, http.StatusBadRequest, "owner required")
		return
	}
	if err := h.save(r.Context(), userID, model.ProviderGitHub, model.GitHubCreds{Owner: owner, InstallationID: instID}); err != nil {
		log.Printf("save github integration: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": model.ProviderGitHub, "configured": true})
}

type gcpReq struct {
	ProjectID       string `json:"project_id"`
	Region          string `json:"region"`
	ArtifactRepo    string `json:"artifact_repo"`
	WIFProvider     string `json:"wif_provider"`
	ServiceAccount  string `json:"service_account"`
	AnthropicAPIKey string `json:"anthropic_api_key"` // optional BYOK; never stored in MySQL
}

// UpsertIntegration handles POST /api/integrations (GCP WIF config, optional BYOK key).
func (h *IntegrationHandler) UpsertIntegration(w http.ResponseWriter, r *http.Request) {
	userID, err := h.userID(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req gcpReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.ProjectID == "" || req.Region == "" || req.WIFProvider == "" || req.ServiceAccount == "" {
		writeErr(w, http.StatusUnprocessableEntity, "project_id, region, wif_provider and service_account are required")
		return
	}
	if req.ArtifactRepo == "" {
		req.ArtifactRepo = "ai-apps"
	}
	creds := model.GCPCreds{
		ProjectID: req.ProjectID, Region: req.Region, ArtifactRepo: req.ArtifactRepo,
		WIFProvider: req.WIFProvider, ServiceAccount: req.ServiceAccount,
	}

	if req.AnthropicAPIKey != "" {
		if h.secrets == nil {
			writeErr(w, http.StatusUnprocessableEntity, "BYOK is not enabled")
			return
		}
		ref, err := h.secrets.PutSecret(r.Context(), "anthropic-key-"+userID, req.AnthropicAPIKey)
		if err != nil {
			log.Printf("store anthropic key: %v", err) // never log the key
			writeErr(w, http.StatusBadGateway, "could not store api key")
			return
		}
		creds.AnthropicSecretRef = ref
	} else if prev := h.loadGCP(r.Context(), userID); prev != nil {
		creds.AnthropicSecretRef = prev.AnthropicSecretRef // keep existing BYOK ref
	}

	if err := h.save(r.Context(), userID, model.ProviderGCP, creds); err != nil {
		log.Printf("save gcp integration: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": model.ProviderGCP, "configured": true, "byok": creds.AnthropicSecretRef != ""})
}

func (h *IntegrationHandler) loadGCP(ctx context.Context, userID string) *model.GCPCreds {
	in, err := h.store.GetIntegration(ctx, userID, model.ProviderGCP)
	if err != nil {
		return nil
	}
	plain, err := crypto.Decrypt(h.key, in.EncryptedCredentials)
	if err != nil {
		return nil
	}
	var c model.GCPCreds
	if json.Unmarshal(plain, &c) != nil {
		return nil
	}
	return &c
}

// ListIntegrations handles GET /api/integrations. It only reports which
// providers are configured; credentials are never returned.
func (h *IntegrationHandler) ListIntegrations(w http.ResponseWriter, r *http.Request) {
	userID, err := h.userID(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	items, err := h.store.ListIntegrations(r.Context(), userID)
	if err != nil {
		log.Printf("list integrations: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	type view struct {
		Provider   string `json:"provider"`
		Configured bool   `json:"configured"`
		CreatedAt  any    `json:"created_at"`
	}
	out := make([]view, 0, len(items))
	for _, it := range items {
		out = append(out, view{it.Provider, true, it.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}
