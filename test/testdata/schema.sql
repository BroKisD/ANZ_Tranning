CREATE TABLE users (
    id VARCHAR(36) PRIMARY KEY,
    name VARCHAR(255),
    email VARCHAR(255) NOT NULL UNIQUE,
    password_hash VARCHAR(255),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);


CREATE TABLE user_integrations (
    id VARCHAR(36) PRIMARY KEY,
    user_id VARCHAR(36) NOT NULL,
    provider VARCHAR(50) NOT NULL,
    encrypted_credentials TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uq_user_provider (user_id, provider),
    CONSTRAINT fk_integrations_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);


CREATE TABLE projects (
    id VARCHAR(36) PRIMARY KEY,
    owner_id VARCHAR(36) NOT NULL,
    name VARCHAR(255),
    prompt TEXT NOT NULL,
    status VARCHAR(50) NOT NULL DEFAULT 'pending',
    current_step VARCHAR(100) NOT NULL DEFAULT 'initializing',
    repo_url VARCHAR(500),
    branch_name VARCHAR(100) DEFAULT 'main',
    pr_url VARCHAR(500),
    live_url VARCHAR(500),
    error_message TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_owner_status (owner_id, status),
    CONSTRAINT fk_projects_owner FOREIGN KEY (owner_id) REFERENCES users(id)
);
