-- Restore the owner role name (#322).
--
-- Mirrors 0007_superuser_role.up.sql: the guard goes back to counting owners,
-- the function and triggers regain their names, and every superuser row moves
-- back to owner.

CREATE OR REPLACE FUNCTION users_guard_last_superuser() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE remaining integer;
BEGIN
  -- Root rows are governed by users_protect_root; counting them here would
  -- refuse the promotion that makes an owner root in the first place.
  IF OLD.role = 'root' THEN
    IF TG_OP = 'DELETE' THEN
      RETURN OLD;
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'DELETE' THEN
    IF OLD.role = 'owner' AND OLD.status = 'active' THEN
      PERFORM pg_advisory_xact_lock(hashtext('gnvipi:owners'));
      SELECT count(*) INTO remaining FROM users
       WHERE role IN ('owner', 'root') AND status = 'active' AND id <> OLD.id;
      IF remaining = 0 THEN
        RAISE EXCEPTION 'last active owner cannot be deleted';
      END IF;
    END IF;
    RETURN OLD;
  END IF;
  IF OLD.role = 'owner' AND OLD.status = 'active'
     AND (NEW.role <> 'owner' OR NEW.status <> 'active') THEN
    PERFORM pg_advisory_xact_lock(hashtext('gnvipi:owners'));
    SELECT count(*) INTO remaining FROM users
     WHERE role IN ('owner', 'root') AND status = 'active' AND id <> OLD.id;
    IF remaining = 0 THEN
      RAISE EXCEPTION 'last active owner cannot be demoted or disabled';
    END IF;
  END IF;
  RETURN NEW;
END $$;

ALTER FUNCTION users_guard_last_superuser() RENAME TO users_guard_last_owner;
ALTER TRIGGER users_guard_last_superuser_bu ON users RENAME TO users_guard_last_owner_bu;
ALTER TRIGGER users_guard_last_superuser_bd ON users RENAME TO users_guard_last_owner_bd;

ALTER TABLE users DROP CONSTRAINT users_role_check;
UPDATE users SET role = 'owner' WHERE role = 'superuser';
ALTER TABLE users ADD CONSTRAINT users_role_check
  CHECK (role IN ('root', 'owner', 'admin', 'user'));