-- Reverses 000001_extensions.up.sql.
--
-- This will fail if any table still has a gen_random_uuid() default, which is
-- correct behaviour, dropping the extension out from under live tables would
-- break every subsequent insert.
DROP EXTENSION IF EXISTS pgcrypto;
