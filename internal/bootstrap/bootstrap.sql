CREATE DATABASE IF NOT EXISTS kubernetes;
CREATE USER IF NOT EXISTS kine;
-- Kine requires sequential revisions. CockroachDB's default unique_rowid()
-- normalization of BIGSERIAL does not provide these.
ALTER ROLE kine SET serial_normalization = 'sql_sequence';
GRANT ALL ON DATABASE kubernetes TO kine;
GRANT ALL ON SCHEMA kubernetes.public TO kine;
