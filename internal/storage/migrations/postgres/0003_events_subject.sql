-- What happened to one item, newest first, for the operator console.
CREATE INDEX events_subject ON events (project, subject_key, seq);
