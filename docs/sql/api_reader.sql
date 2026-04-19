-- Creates the read-only role used by cmd/api. Intended to be applied
-- once per environment by an operator with superuser / owner rights,
-- NOT by the bot's goose migrations — the role is a deployment-time
-- concern (credential choice varies per install) rather than a schema
-- concern.
--
-- Re-running this file is safe: role creation is guarded, and GRANTs
-- are idempotent.
--
-- Two credential paths are supported:
--
--   1. Password — uncomment the ALTER ROLE ... WITH PASSWORD line and
--      substitute the desired value (or manage it out of band).
--
--   2. AWS RDS IAM auth — GRANT rds_iam TO api_reader; the API then
--      connects using short-lived IAM-signed tokens the same way the
--      bot does (secrets.PostgresIAMAuth).
--
-- The role is granted SELECT only. CONNECT on the database and USAGE
-- on the schema are necessary even for read-only access; everything
-- else is deliberately withheld.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'api_reader') THEN
        CREATE ROLE api_reader LOGIN;
    END IF;
END
$$;

-- 1. Password auth — uncomment and set:
-- ALTER ROLE api_reader WITH PASSWORD 'change-me';

-- 2. RDS IAM auth — uncomment on RDS/Aurora:
-- GRANT rds_iam TO api_reader;

-- Substitute the database name if yours differs. :DBNAME works with
-- psql \connect-style variable substitution.
GRANT CONNECT ON DATABASE deploy_bot TO api_reader;
GRANT USAGE ON SCHEMA public TO api_reader;

GRANT SELECT ON TABLE history TO api_reader;
GRANT SELECT ON TABLE pending_deploys TO api_reader;

-- Future tables created by later goose migrations won't automatically
-- be visible to api_reader. This default-privileges grant makes every
-- new table in the public schema SELECT-able by api_reader, but only
-- when created by the role that owns the existing tables (typically
-- the bot's read/write user). Adjust FOR ROLE to match your owner.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT ON TABLES TO api_reader;
