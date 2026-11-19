package service

import (
	"context"
	"fmt"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

// SecretManager stores BYOK secrets in GCP Secret Manager.
type SecretManager struct {
	client  *secretmanager.Client
	project string
}

func NewSecretManager(ctx context.Context, project string) (*SecretManager, error) {
	c, err := secretmanager.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return &SecretManager{client: c, project: project}, nil
}

func (m *SecretManager) Close() error { return m.client.Close() }

// PutSecret creates the secret if needed, adds value as a new version and
// returns the `.../versions/latest` reference. The value is never logged.
func (m *SecretManager) PutSecret(ctx context.Context, secretID, value string) (string, error) {
	parent := "projects/" + m.project
	name := fmt.Sprintf("%s/secrets/%s", parent, secretID)

	if _, err := m.client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name}); err != nil {
		_, err = m.client.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
			Parent:   parent,
			SecretId: secretID,
			Secret: &secretmanagerpb.Secret{
				Replication: &secretmanagerpb.Replication{
					Replication: &secretmanagerpb.Replication_Automatic_{
						Automatic: &secretmanagerpb.Replication_Automatic{},
					},
				},
			},
		})
		if err != nil {
			return "", fmt.Errorf("create secret: %w", err)
		}
	}
	if _, err := m.client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  name,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)},
	}); err != nil {
		return "", fmt.Errorf("add secret version: %w", err)
	}
	return name + "/versions/latest", nil
}
