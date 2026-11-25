-- Shared test license supplied for the prototype; replace this default for real deployments.
SET CLUSTER SETTING enterprise.license = 'crl-0-ELWog+YGGAMyEKAq1OlLJEcLu7r0Zufaq9M6EJWSCcMaZUAgiNbpHrAYa+0';
-- Keep application and critical system data on the same three-voter baseline.
-- CockroachDB otherwise defaults several system zones to five replicas.
ALTER RANGE default CONFIGURE ZONE USING num_replicas = 3, num_voters = 3;
ALTER DATABASE system CONFIGURE ZONE USING num_replicas = 3, num_voters = 3;
ALTER RANGE meta CONFIGURE ZONE USING num_replicas = 3, num_voters = 3;
ALTER RANGE system CONFIGURE ZONE USING num_replicas = 3, num_voters = 3;
ALTER RANGE liveness CONFIGURE ZONE USING num_replicas = 3, num_voters = 3;
ALTER RANGE timeseries CONFIGURE ZONE USING num_replicas = 3, num_voters = 3;
CREATE DATABASE IF NOT EXISTS kubernetes;
CREATE USER IF NOT EXISTS kine;
SET CLUSTER SETTING sql.txn.read_committed_isolation.enabled = true;
ALTER ROLE kine SET default_transaction_isolation = 'read committed';
ALTER DATABASE kubernetes SET default_transaction_isolation = 'read committed';
-- Kine requires sequential revisions. CockroachDB's default unique_rowid()
-- normalization of BIGSERIAL does not provide these.
ALTER ROLE kine SET serial_normalization = 'sql_sequence';
GRANT ALL ON DATABASE kubernetes TO kine;
GRANT ALL ON SCHEMA kubernetes.public TO kine;

-- Block SQL login before enabling the shared DB Console password.
CREATE USER IF NOT EXISTS dashboard;
GRANT SYSTEM NOSQLLOGIN TO dashboard;
ALTER USER dashboard WITH PASSWORD 'roachking';
