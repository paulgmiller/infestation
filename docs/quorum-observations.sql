-- Read-only CockroachDB v26.2 incident collection. Run statements individually
-- so an unavailable query does not prevent saving later observations.
-- Use an existing authorized operator session; do not distribute admin keys.
SET statement_timeout = '15s';
SELECT now() AS observed_at, version() AS gateway_version;
SHOW CLUSTER SETTING server.time_until_store_dead;
SHOW CLUSTER SETTING server.time_after_store_suspect;
SHOW CLUSTER SETTING server.time_after_store_suspect_in_store_liveness;
SELECT * FROM crdb_internal.gossip_nodes;
SELECT * FROM crdb_internal.gossip_liveness;
SELECT * FROM crdb_internal.kv_node_status;
SELECT * FROM crdb_internal.kv_store_status;
SHOW ZONE CONFIGURATIONS;
-- Small-cluster capture of every range, including non-table system ranges.
-- On a large cluster add WHERE range_id IN (...) for the incident range IDs.
SELECT * FROM crdb_internal.ranges_no_leases;
SELECT now() AS observation_finished_at;
