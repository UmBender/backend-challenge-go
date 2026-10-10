-- Restores the PostgreSQL default for the public schema.
REVOKE USAGE ON SCHEMA public FROM wallet_app;
GRANT USAGE ON SCHEMA public TO PUBLIC;
