package store

const schemaV4 = `
CREATE TABLE services (
 service_id TEXT PRIMARY KEY,
 coverage_start TEXT NOT NULL
);
CREATE TABLE reservation_clock (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 high_water TEXT NOT NULL
);
CREATE TABLE reservations (
 reservation_id INTEGER PRIMARY KEY AUTOINCREMENT,
 service_id TEXT NOT NULL REFERENCES services(service_id),
 container_id TEXT NOT NULL CHECK(length(container_id)=64 AND container_id NOT GLOB '*[^0-9a-f]*'),
 reserved_at TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('planned','dispatching','verifying','succeeded','failed','uncertain','canceled')),
 max_attempts INTEGER NOT NULL CHECK(max_attempts>0),
 window_ns INTEGER NOT NULL CHECK(window_ns>0),
 cooldown_ns INTEGER NOT NULL CHECK(cooldown_ns>0),
 assessment_id INTEGER,
 assessment_summary TEXT
);
CREATE INDEX reservations_service_time ON reservations(service_id,reserved_at DESC,reservation_id DESC);
CREATE INDEX reservations_time ON reservations(reserved_at);
CREATE UNIQUE INDEX reservations_one_unresolved ON reservations((1)) WHERE state IN ('planned','dispatching','verifying','uncertain');
PRAGMA user_version=4;
`
