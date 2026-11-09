CREATE SCHEMA IF NOT EXISTS favorites;
CREATE TABLE IF NOT EXISTS favorites.owners(owner_key text PRIMARY KEY,closure_id text);
CREATE TABLE IF NOT EXISTS favorites.folders(owner_key text NOT NULL REFERENCES favorites.owners(owner_key),id text NOT NULL,name text NOT NULL CHECK(char_length(name)<=20),is_default boolean NOT NULL,revision bigint NOT NULL CHECK(revision>0),PRIMARY KEY(owner_key,id));
CREATE UNIQUE INDEX IF NOT EXISTS favorites_folder_names ON favorites.folders(owner_key,name COLLATE "C");
CREATE UNIQUE INDEX IF NOT EXISTS favorites_one_default ON favorites.folders(owner_key) WHERE is_default;
CREATE TABLE IF NOT EXISTS favorites.items(owner_key text NOT NULL,id text NOT NULL,kind text NOT NULL CHECK(kind IN('course','teacher','resource')),target_id text NOT NULL,canonical_target_id text NOT NULL DEFAULT '',title text NOT NULL CHECK(char_length(title)<=500),folder_id text NOT NULL,revision bigint NOT NULL CHECK(revision>0),created_at timestamptz NOT NULL,PRIMARY KEY(owner_key,id),UNIQUE(owner_key,kind,target_id),FOREIGN KEY(owner_key,folder_id) REFERENCES favorites.folders(owner_key,id));
CREATE INDEX IF NOT EXISTS favorites_items_newest ON favorites.items(owner_key,created_at DESC,id);
CREATE INDEX IF NOT EXISTS favorites_items_name ON favorites.items(owner_key,title COLLATE "C",id);
CREATE INDEX IF NOT EXISTS favorites_items_folder ON favorites.items(owner_key,folder_id);
CREATE TABLE IF NOT EXISTS favorites.requests(owner_key text NOT NULL,key text NOT NULL,digest text NOT NULL,result jsonb NOT NULL,PRIMARY KEY(owner_key,key));
CREATE TABLE IF NOT EXISTS favorites.events(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,owner_key text NOT NULL,kind text NOT NULL,operation text NOT NULL,created_at timestamptz NOT NULL DEFAULT now());

REVOKE ALL ON SCHEMA favorites FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA favorites FROM PUBLIC;

-- Provider-owned daily aggregates and immutable versions used by rebuild cursors.
CREATE TABLE IF NOT EXISTS favorites.activity_buckets(id text PRIMARY KEY,kind text NOT NULL CHECK(kind IN('course','teacher')),target_id text NOT NULL,school_id text NOT NULL,day date NOT NULL,first_count bigint NOT NULL,second_count bigint NOT NULL,revision bigint NOT NULL CHECK(revision>0),visible boolean NOT NULL,data jsonb NOT NULL DEFAULT '{}',UNIQUE(kind,target_id,day));
CREATE INDEX IF NOT EXISTS favorites_activity_target ON favorites.activity_buckets(kind,target_id,day);
CREATE TABLE IF NOT EXISTS favorites.activity_history(id text NOT NULL,revision bigint NOT NULL,day date NOT NULL,data jsonb NOT NULL,recorded_xid xid8 NOT NULL DEFAULT pg_current_xact_id(),PRIMARY KEY(id,revision));
CREATE INDEX IF NOT EXISTS favorites_activity_history_page ON favorites.activity_history(id COLLATE "C",revision DESC);
CREATE INDEX IF NOT EXISTS favorites_activity_history_day ON favorites.activity_history(day,id,revision);
CREATE TABLE IF NOT EXISTS favorites.activity_retention(singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),revision bigint NOT NULL DEFAULT 0);
INSERT INTO favorites.activity_retention(singleton) VALUES(true) ON CONFLICT DO NOTHING;
CREATE OR REPLACE FUNCTION favorites.invalidate_activity() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN UPDATE favorites.activity_retention SET revision=revision+1 WHERE singleton;RETURN NULL;END $$;
REVOKE ALL ON FUNCTION favorites.invalidate_activity() FROM PUBLIC;
CREATE OR REPLACE TRIGGER invalidate_activity AFTER UPDATE OR DELETE ON favorites.activity_history FOR EACH ROW EXECUTE FUNCTION favorites.invalidate_activity();
CREATE OR REPLACE TRIGGER truncate_activity AFTER TRUNCATE ON favorites.activity_history FOR EACH STATEMENT EXECUTE FUNCTION favorites.invalidate_activity();
REVOKE ALL ON TABLE favorites.activity_buckets,favorites.activity_history,favorites.activity_retention FROM PUBLIC;

CREATE TABLE IF NOT EXISTS favorites.outbox(id text PRIMARY KEY,data jsonb NOT NULL,seq bigint GENERATED ALWAYS AS IDENTITY UNIQUE,sent_at timestamptz);
CREATE INDEX IF NOT EXISTS favorites_outbox_pending ON favorites.outbox((data->>'source') COLLATE "C",(data->>'subject') COLLATE "C",seq) WHERE sent_at IS NULL;
REVOKE ALL ON favorites.outbox FROM PUBLIC;
