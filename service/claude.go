package service

import "context"

const maxFixRetries = 3

type Claude struct{}

// GenerateAndTest creates /tmp/workspaces/<projectID>, runs `claude -p` with a
// timeout, then loops up to maxFixRetries times over lint/tests feeding failures back.
// Returns the workspace dir (caller cleans up after the job).
func (c *Claude) GenerateAndTest(ctx context.Context, projectID, prompt string) (workspaceDir string, err error) {
	return "", ErrNotImplemented
}
