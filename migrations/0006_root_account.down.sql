-- Reverses 0006: removes the root guards, then the role and the row.
DROP TRIGGER IF EXISTS users_protect_root_bu ON users;
DROP TRIGGER IF EXISTS users_protect_root_bd ON users;
DROP FUNCTION IF EXISTS users_protect_root();

DELETE FROM users WHERE role = 'root';
DROP INDEX IF EXISTS users_single_root;

ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
  CHECK (role IN ('owner', 'admin', 'user'));

CREATE OR REPLACE FUNCTION users_guard_last_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE remaining integer;
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.role = 'owner' AND OLD.status = 'active' THEN
      PERFORM pg_advisory_xact_lock(hashtext('gnvipi:owners'));
      SELECT count(*) INTO remaining FROM users
       WHERE role = 'owner' AND status = 'active' AND id <> OLD.id;
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
     WHERE role = 'owner' AND status = 'active' AND id <> OLD.id;
    IF remaining = 0 THEN
      RAISE EXCEPTION 'last active owner cannot be demoted or disabled';
    END IF;
  END IF;
  RETURN NEW;
END $$;