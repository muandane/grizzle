CREATE TABLE metrics (
    id BIGSERIAL PRIMARY KEY,
    service_name VARCHAR(100) NOT NULL,
    cpu_usage DOUBLE PRECISION NOT NULL,
    memory_mb INT NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_metrics_service_recorded ON metrics(service_name, recorded_at DESC);
