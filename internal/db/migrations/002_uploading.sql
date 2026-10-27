-- Presigned (direct-to-bucket) uploads: a job exists in 'uploading' from the moment the API hands
-- out the upload form until POST /jobs/{id}/complete has verified the object and queued the job.
ALTER TABLE jobs DROP CONSTRAINT jobs_status_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_check
  CHECK (status IN ('uploading','queued','probing','running','done','failed','canceled'));
-- Size the client announced for a presigned upload (NULL for multipart uploads). Only ever
-- compared with the stored object's size; it is never trusted as the size itself.
ALTER TABLE jobs ADD COLUMN declared_size bigint;
CREATE INDEX jobs_uploading_idx ON jobs (updated_at) WHERE status = 'uploading';
