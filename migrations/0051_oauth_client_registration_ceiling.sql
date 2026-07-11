-- Dynamic OAuth registration is public and creates durable rows. The application
-- enforces a rolling-hour ceiling under a database advisory lock, shared by every
-- server replica. This index keeps that count bounded to the current window rather
-- than scanning the full registration history as the table grows.
CREATE INDEX idx_oauth_clients_created_at ON oauth_clients(created_at);
