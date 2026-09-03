-- pgcrypto provides gen_random_uuid(), which every table's primary key
-- default depends on. Nothing else can be created until this exists, so it is
-- deliberately the first migration.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
