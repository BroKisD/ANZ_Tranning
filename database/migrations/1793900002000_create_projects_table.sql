-- +goose Up
-- +goose StatementBegin
CREATE TABLE projects (
    id VARCHAR(36) PRIMARY KEY,
    owner_id VARCHAR(36) NOT NULL,
    name VARCHAR(255),
    prompt TEXT NOT NULL,
    status VARCHAR(50) NOT NULL DEFAULT 'pending' COMMENT 'pending | generating | waiting_for_approval | deploying | success | failed',
    current_step VARCHAR(100) NOT NULL DEFAULT 'initializing' COMMENT 'initializing | repo_created | tests_passed | merging | deploying',
    repo_url VARCHAR(500),
    branch_name VARCHAR(100) DEFAULT 'main',
    pr_url VARCHAR(500),
    live_url VARCHAR(500),
    error_message TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_owner_status (owner_id, status),
    CONSTRAINT fk_projects_owner FOREIGN KEY (owner_id) REFERENCES users(id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE projects;
-- +goose StatementEnd
