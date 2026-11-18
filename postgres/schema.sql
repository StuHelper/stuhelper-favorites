CREATE SCHEMA IF NOT EXISTS favorites;
CREATE TABLE IF NOT EXISTS favorites.owners(owner_key text PRIMARY KEY,closure_id text);
CREATE TABLE IF NOT EXISTS favorites.folders(owner_key text NOT NULL REFERENCES favorites.owners(owner_key),id text NOT NULL,name text NOT NULL CHECK(char_length(name)<=20),is_default boolean NOT NULL,revision bigint NOT NULL CHECK(revision>0),PRIMARY KEY(owner_key,id));
CREATE UNIQUE INDEX IF NOT EXISTS favorites_folder_names ON favorites.folders(owner_key,name COLLATE "C");
CREATE UNIQUE INDEX IF NOT EXISTS favorites_one_default ON favorites.folders(owner_key) WHERE is_default;
CREATE TABLE IF NOT EXISTS favorites.items(owner_key text NOT NULL,id text NOT NULL,kind text NOT NULL CHECK(kind IN('course','teacher','resource')),target_id text NOT NULL,canonical_target_id text NOT NULL DEFAULT '',title text NOT NULL CHECK(char_length(title)<=500),folder_id text NOT NULL,revision bigint NOT NULL CHECK(revision>0),created_at timestamptz NOT NULL,PRIMARY KEY(owner_key,id),UNIQUE(owner_key,kind,target_id),FOREIGN KEY(owner_key,folder_id) REFERENCES favorites.folders(owner_key,id));
ALTER TABLE favorites.items ADD COLUMN IF NOT EXISTS canonical_target_id text NOT NULL DEFAULT '';
UPDATE favorites.items SET canonical_target_id=target_id WHERE canonical_target_id='';
DO $$ BEGIN IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='favorites_items_canonical_check' AND conrelid='favorites.items'::regclass) THEN ALTER TABLE favorites.items ADD CONSTRAINT favorites_items_canonical_check CHECK(canonical_target_id<>'' AND char_length(canonical_target_id)<=200); END IF; END $$;
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
CREATE INDEX IF NOT EXISTS favorites_activity_window ON favorites.activity_buckets(kind,day,target_id) INCLUDE(first_count) WHERE visible;
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
CREATE INDEX IF NOT EXISTS favorites_outbox_pending_seq ON favorites.outbox(seq) WHERE sent_at IS NULL;
DROP INDEX IF EXISTS favorites.favorites_outbox_pending;
REVOKE ALL ON favorites.outbox FROM PUBLIC;

-- Deduplicate once, after the activity tables exist. A removed favorite has the
-- same UTC accounting day as an ordinary cancellation, including across days.
DO $$
DECLARE
    removed_target record;
    migration_time timestamptz := transaction_timestamp();
    bucket_day date := (migration_time AT TIME ZONE 'UTC')::date;
    bucket_id text;
    net_count bigint;
    bucket_revision bigint;
    bucket_visible boolean;
    snapshot jsonb;
    event_id text;
BEGIN
    IF to_regclass('favorites.favorites_items_canonical') IS NOT NULL THEN RETURN; END IF;
    FOR removed_target IN
        WITH ranked AS (
            SELECT owner_key,id,row_number() OVER (
                PARTITION BY owner_key,kind,canonical_target_id ORDER BY created_at,id COLLATE "C"
            ) AS position FROM favorites.items
        ), removed AS (
            DELETE FROM favorites.items i USING ranked r
            WHERE r.position>1 AND i.owner_key=r.owner_key AND i.id=r.id
            RETURNING i.kind,i.canonical_target_id
        )
        SELECT kind,canonical_target_id,count(*) AS amount FROM removed
        WHERE kind IN ('course','teacher') GROUP BY kind,canonical_target_id
        ORDER BY kind,canonical_target_id
    LOOP
        bucket_id := removed_target.kind || '/' ||
            translate(encode(convert_to(removed_target.canonical_target_id,'UTF8'),'base64'), E'+/=\n','-_') ||
            '/' || to_char(bucket_day,'YYYY-MM-DD');
        INSERT INTO favorites.activity_buckets(id,kind,target_id,school_id,day,first_count,second_count,revision,visible)
        VALUES(bucket_id,removed_target.kind,removed_target.canonical_target_id,'',bucket_day,-removed_target.amount,0,1,false)
        ON CONFLICT(id) DO UPDATE SET first_count=favorites.activity_buckets.first_count+EXCLUDED.first_count,
                                      revision=favorites.activity_buckets.revision+1
        RETURNING greatest(first_count,0),revision,(first_count>0 OR second_count>0)
        INTO net_count,bucket_revision,bucket_visible;

        snapshot := jsonb_build_object(
            'source','favorites','id',bucket_id,'day',to_char(bucket_day,'YYYY-MM-DD'),
            'target',jsonb_build_object('source','catalog','kind',removed_target.kind,'id',removed_target.canonical_target_id),
            'revision',bucket_revision,'visible',bucket_visible,'metrics',jsonb_build_object('favorites',net_count));
        UPDATE favorites.activity_buckets SET visible=bucket_visible,data=snapshot WHERE id=bucket_id;
        INSERT INTO favorites.activity_history(id,revision,day,data) VALUES(bucket_id,bucket_revision,bucket_day,snapshot);
        event_id := 'activity:' || encode(sha256(convert_to(bucket_id,'UTF8')),'hex') || ':' || bucket_revision;
        INSERT INTO favorites.outbox(id,data) VALUES(event_id,jsonb_build_object(
            'specversion','1.0','id',event_id,'source','favorites','type','discovery.activity.v1',
            'subject',bucket_id,'time',migration_time,'data',snapshot));
        PERFORM pg_notify('favorites_outbox','');
    END LOOP;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS favorites_items_canonical ON favorites.items(owner_key,kind,canonical_target_id) WHERE canonical_target_id<>'';
