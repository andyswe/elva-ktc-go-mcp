-- Read-only database role for elva-mcp-stdio. Run as a PostgreSQL superuser in the
-- ElVa database, after ElVa has created its table:
--   sudo -u postgres psql -d elva -v pw="'a-strong-password'" -f deploy/readonly-role.sql
CREATE ROLE elva_ro LOGIN PASSWORD :pw;
GRANT CONNECT ON DATABASE elva TO elva_ro;
GRANT USAGE ON SCHEMA public TO elva_ro;
GRANT SELECT ON measurement TO elva_ro;
-- Extra safety: even a forgotten read-only flag can't write, and runaway queries stop.
ALTER ROLE elva_ro SET default_transaction_read_only = on;
ALTER ROLE elva_ro SET statement_timeout = '60s';
