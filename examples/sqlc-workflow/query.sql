-- name: CreateOrganization :one
INSERT INTO organizations (name)
VALUES ($1)
RETURNING id, name, created_at;

-- name: CreateUser :one
INSERT INTO users (org_id, email, full_name)
VALUES ($1, $2, $3)
RETURNING id, org_id, email, full_name, created_at;

-- name: GetUserByEmail :one
SELECT id, org_id, email, full_name, created_at
FROM users
WHERE email = $1;

-- name: ListUsersByOrg :many
SELECT id, org_id, email, full_name, created_at
FROM users
WHERE org_id = $1
ORDER BY id;
