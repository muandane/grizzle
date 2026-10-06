package db

import (
	"context"
)

const createOrganization = `-- name: CreateOrganization :one
INSERT INTO organizations (name)
VALUES ($1)
RETURNING id, name, created_at
`

func (q *Queries) CreateOrganization(ctx context.Context, name string) (Organization, error) {
	row := q.db.QueryRowContext(ctx, createOrganization, name)
	var i Organization
	err := row.Scan(&i.ID, &i.Name, &i.CreatedAt)
	return i, err
}

const createUser = `-- name: CreateUser :one
INSERT INTO users (org_id, email, full_name)
VALUES ($1, $2, $3)
RETURNING id, org_id, email, full_name, created_at
`

type CreateUserParams struct {
	OrgID    int64  `json:"org_id"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
}

func (q *Queries) CreateUser(ctx context.Context, arg CreateUserParams) (User, error) {
	row := q.db.QueryRowContext(ctx, createUser, arg.OrgID, arg.Email, arg.FullName)
	var i User
	err := row.Scan(
		&i.ID,
		&i.OrgID,
		&i.Email,
		&i.FullName,
		&i.CreatedAt,
	)
	return i, err
}

const getUserByEmail = `-- name: GetUserByEmail :one
SELECT id, org_id, email, full_name, created_at
FROM users
WHERE email = $1
`

func (q *Queries) GetUserByEmail(ctx context.Context, email string) (User, error) {
	row := q.db.QueryRowContext(ctx, getUserByEmail, email)
	var i User
	err := row.Scan(
		&i.ID,
		&i.OrgID,
		&i.Email,
		&i.FullName,
		&i.CreatedAt,
	)
	return i, err
}

const listUsersByOrg = `-- name: ListUsersByOrg :many
SELECT id, org_id, email, full_name, created_at
FROM users
WHERE org_id = $1
ORDER BY id
`

func (q *Queries) ListUsersByOrg(ctx context.Context, orgID int64) ([]User, error) {
	rows, err := q.db.QueryContext(ctx, listUsersByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var items []User
	for rows.Next() {
		var i User
		if err := rows.Scan(
			&i.ID,
			&i.OrgID,
			&i.Email,
			&i.FullName,
			&i.CreatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
