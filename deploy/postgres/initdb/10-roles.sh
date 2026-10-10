#!/bin/sh
# Runs once, when the PostgreSQL data volume is first initialised (official
# image entrypoint, as the bootstrap superuser). Creates two roles:
#
#   wallet_owner  owns the database and its objects; runs migrations.
#                 NOSUPERUSER, so ledger triggers also bind it.
#   wallet_app    used by the service at runtime. Gets only the privileges
#                 that migrations grant explicitly, table by table.
#
# Changing this file does not affect an existing volume:
# run `docker compose down -v` to re-initialise.
set -eu

: "${WALLET_OWNER_PASSWORD:?WALLET_OWNER_PASSWORD is required}"
: "${WALLET_APP_PASSWORD:?WALLET_APP_PASSWORD is required}"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
    -v owner_password="$WALLET_OWNER_PASSWORD" \
    -v app_password="$WALLET_APP_PASSWORD" <<'SQL'
CREATE ROLE wallet_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'owner_password';
CREATE ROLE wallet_app   LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'app_password';
CREATE DATABASE wallet OWNER wallet_owner;
REVOKE ALL ON DATABASE wallet FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE wallet TO wallet_app;
SQL
