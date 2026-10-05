CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT NOT NULL,
    created_at TEXT DEFAULT 'now()'
);

CREATE TABLE posts (
    id INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    content TEXT,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX idx_posts_user_id ON posts(user_id);
