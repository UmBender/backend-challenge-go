-- Baseline least-privilege setup for the public schema. Runs as wallet_owner,
-- who owns the database and therefore the public schema.
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO wallet_app;
