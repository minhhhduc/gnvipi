-- Built-in root account (#318).
--
-- Root sits above owner: one account named gnvipi exists on every install and
-- no user-management path can delete, rename, demote or disable it. The row is
-- guarded in the database so the invariant also holds for direct SQL.

ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
  CHECK (role IN ('root', 'owner', 'admin', 'user'));

-- Exactly one root row. The expression is constant, so the index only has to
-- detect a second row in the filtered set.
CREATE UNIQUE INDEX users_single_root ON users ((true)) WHERE role = 'root';

CREATE OR REPLACE FUNCTION users_guard_last_owner() RETURNS trigger
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

CREATE OR REPLACE FUNCTION users_protect_root() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'root account cannot be deleted';
  END IF;
  IF NEW.username <> OLD.username THEN
    RAISE EXCEPTION 'root account cannot be renamed';
  END IF;
  IF NEW.role <> 'root' THEN
    RAISE EXCEPTION 'root account cannot be demoted';
  END IF;
  IF NEW.status <> 'active' THEN
    RAISE EXCEPTION 'root account cannot be disabled';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER users_protect_root_bu
BEFORE UPDATE OF username, role, status ON users
FOR EACH ROW WHEN (OLD.role = 'root')
EXECUTE FUNCTION users_protect_root();

CREATE TRIGGER users_protect_root_bd
BEFORE DELETE ON users
FOR EACH ROW WHEN (OLD.role = 'root')
EXECUTE FUNCTION users_protect_root();