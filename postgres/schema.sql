CREATE SCHEMA IF NOT EXISTS favorites;
CREATE TABLE IF NOT EXISTS favorites.owners(owner_key text PRIMARY KEY,import_after text NOT NULL DEFAULT '',import_done boolean NOT NULL DEFAULT false);
CREATE TABLE IF NOT EXISTS favorites.folders(owner_key text NOT NULL REFERENCES favorites.owners(owner_key),id text NOT NULL,name text NOT NULL CHECK(char_length(name)<=20),is_default boolean NOT NULL,revision bigint NOT NULL CHECK(revision>0),PRIMARY KEY(owner_key,id));
CREATE UNIQUE INDEX IF NOT EXISTS favorites_folder_names ON favorites.folders(owner_key,name COLLATE "C");
CREATE UNIQUE INDEX IF NOT EXISTS favorites_one_default ON favorites.folders(owner_key) WHERE is_default;
CREATE TABLE IF NOT EXISTS favorites.items(owner_key text NOT NULL,id text NOT NULL,kind text NOT NULL CHECK(kind IN('course','teacher','resource')),target_id text NOT NULL,title text NOT NULL CHECK(char_length(title)<=500),folder_id text NOT NULL,revision bigint NOT NULL CHECK(revision>0),created_at timestamptz NOT NULL,PRIMARY KEY(owner_key,id),UNIQUE(owner_key,kind,target_id),FOREIGN KEY(owner_key,folder_id) REFERENCES favorites.folders(owner_key,id));
CREATE INDEX IF NOT EXISTS favorites_items_newest ON favorites.items(owner_key,created_at DESC,id);
CREATE INDEX IF NOT EXISTS favorites_items_name ON favorites.items(owner_key,title COLLATE "C",id);
CREATE INDEX IF NOT EXISTS favorites_items_folder ON favorites.items(owner_key,folder_id);
CREATE TABLE IF NOT EXISTS favorites.requests(owner_key text NOT NULL,key text NOT NULL,digest text NOT NULL,result jsonb NOT NULL,PRIMARY KEY(owner_key,key));
CREATE TABLE IF NOT EXISTS favorites.events(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,owner_key text NOT NULL,kind text NOT NULL,operation text NOT NULL,created_at timestamptz NOT NULL DEFAULT now());

REVOKE ALL ON SCHEMA favorites FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA favorites FROM PUBLIC;

ALTER TABLE favorites.owners ADD COLUMN IF NOT EXISTS closure_id text;
