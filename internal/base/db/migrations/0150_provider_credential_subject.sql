ALTER TABLE weave_provider_credentials
 ADD COLUMN credential_scope TEXT NOT NULL,
 ADD COLUMN credential_user_id TEXT NOT NULL,
 ADD COLUMN credential_service_id TEXT NOT NULL,
 ADD CONSTRAINT weave_provider_credential_subject_shape CHECK (
  (credential_scope='user' AND credential_user_id<>'' AND credential_service_id='') OR
  (credential_scope='workspace_service' AND credential_user_id='' AND credential_service_id<>'')
 );

-- A resource identifier cannot be reassigned to a different credential owner.
-- Key rotation remains possible without changing a published reference.
CREATE FUNCTION weave_provider_credential_subject_immutable() RETURNS trigger AS $$
BEGIN
 IF (NEW.credential_scope,NEW.credential_user_id,NEW.credential_service_id)
   IS DISTINCT FROM (OLD.credential_scope,OLD.credential_user_id,OLD.credential_service_id) THEN
  RAISE EXCEPTION 'provider credential subject is immutable';
 END IF;
 RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER weave_provider_credential_subject_immutable
 BEFORE UPDATE ON weave_provider_credentials
 FOR EACH ROW EXECUTE FUNCTION weave_provider_credential_subject_immutable();
