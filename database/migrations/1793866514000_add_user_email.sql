-- +goose Up
ALTER TABLE users ADD COLUMN email VARCHAR(255) UNIQUE NOT NULL; 

-- +goose Down
ALTER TABLE users DROP COLUMN email;
