CREATE TABLE agent_port_leases (
    run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    base INTEGER NOT NULL CHECK (base BETWEEN 1 AND 65535),
    size INTEGER NOT NULL CHECK (size >= 2 AND base + size <= 65536)
);
