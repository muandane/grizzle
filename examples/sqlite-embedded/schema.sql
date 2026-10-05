-- Declarative SQLite schema
CREATE TABLE devices (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    serial_number TEXT NOT NULL UNIQUE,
    firmware_version TEXT NOT NULL,
    last_seen_at TEXT DEFAULT 'CURRENT_TIMESTAMP'
);

CREATE TABLE telemetry_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id INTEGER NOT NULL,
    temperature REAL NOT NULL,
    battery_level INTEGER NOT NULL,
    recorded_at TEXT DEFAULT 'CURRENT_TIMESTAMP',
    FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE
);

CREATE INDEX idx_telemetry_device ON telemetry_logs (device_id);
