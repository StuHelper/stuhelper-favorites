CREATE SCHEMA IF NOT EXISTS favorites;
CREATE TABLE IF NOT EXISTS favorites.owners(owner_key text PRIMARY KEY,closure_id text);
CREATE TABLE IF NOT EXISTS favorites.folders(owner_key text NOT NULL REFERENCES favorites.owners(owner_key),id text NOT NULL,name text NOT NULL CHECK(char_length(name)<=20),is_default boolean NOT NULL,revision bigint NOT NULL CHECK(revision>0),position integer NOT NULL DEFAULT 0,created_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(owner_key,id));
ALTER TABLE favorites.folders ADD COLUMN IF NOT EXISTS position integer NOT NULL DEFAULT 0;
ALTER TABLE favorites.folders ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
CREATE UNIQUE INDEX IF NOT EXISTS favorites_folder_names ON favorites.folders(owner_key,name COLLATE "C");
CREATE UNIQUE INDEX IF NOT EXISTS favorites_one_default ON favorites.folders(owner_key) WHERE is_default;
CREATE INDEX IF NOT EXISTS favorites_folders_order ON favorites.folders(owner_key,is_default DESC,position ASC,created_at ASC,id COLLATE "C" ASC);
CREATE TABLE IF NOT EXISTS favorites.items(owner_key text NOT NULL,id text NOT NULL,kind text NOT NULL CHECK(kind IN('course','teacher','resource')),target_id text NOT NULL,canonical_target_id text NOT NULL DEFAULT '',title text NOT NULL CHECK(char_length(title)<=500),folder_id text NOT NULL,revision bigint NOT NULL CHECK(revision>0),created_at timestamptz NOT NULL,PRIMARY KEY(owner_key,id),UNIQUE(owner_key,kind,target_id),FOREIGN KEY(owner_key,folder_id) REFERENCES favorites.folders(owner_key,id));
ALTER TABLE favorites.items ADD COLUMN IF NOT EXISTS canonical_target_id text NOT NULL DEFAULT '';
UPDATE favorites.items SET canonical_target_id=target_id WHERE canonical_target_id='';
DO $$ BEGIN IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='favorites_items_canonical_check' AND conrelid='favorites.items'::regclass) THEN ALTER TABLE favorites.items ADD CONSTRAINT favorites_items_canonical_check CHECK(canonical_target_id<>'' AND char_length(canonical_target_id)<=200); END IF; END $$;
CREATE INDEX IF NOT EXISTS favorites_items_newest ON favorites.items(owner_key,created_at DESC,id);
CREATE INDEX IF NOT EXISTS favorites_items_name ON favorites.items(owner_key,title COLLATE "C",id);
CREATE INDEX IF NOT EXISTS favorites_items_folder ON favorites.items(owner_key,folder_id);
CREATE TABLE IF NOT EXISTS favorites.requests(owner_key text NOT NULL,key text NOT NULL,digest text NOT NULL,result jsonb NOT NULL,PRIMARY KEY(owner_key,key));
CREATE TABLE IF NOT EXISTS favorites.events(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,owner_key text NOT NULL,kind text NOT NULL,operation text NOT NULL,created_at timestamptz NOT NULL DEFAULT now());

-- Unknown completion times of legacy receipts start a full window at migration.
ALTER TABLE favorites.requests ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS favorites_requests_expiry ON favorites.requests(created_at,owner_key,key);
CREATE INDEX IF NOT EXISTS favorites_events_expiry ON favorites.events(created_at,id);

REVOKE ALL ON SCHEMA favorites FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA favorites FROM PUBLIC;

-- Provider-owned daily aggregates and immutable versions used by rebuild cursors.
CREATE TABLE IF NOT EXISTS favorites.activity_buckets(id text PRIMARY KEY,kind text NOT NULL CHECK(kind IN('course','teacher')),target_id text NOT NULL,day date NOT NULL,first_count bigint NOT NULL,revision bigint NOT NULL CHECK(revision>0),visible boolean NOT NULL,data jsonb NOT NULL DEFAULT '{}',UNIQUE(kind,target_id,day));
ALTER TABLE favorites.activity_buckets DROP COLUMN IF EXISTS school_id,DROP COLUMN IF EXISTS second_count;
CREATE INDEX IF NOT EXISTS favorites_activity_target ON favorites.activity_buckets(kind,target_id,day);
CREATE INDEX IF NOT EXISTS favorites_activity_window ON favorites.activity_buckets(kind,day,target_id) INCLUDE(first_count) WHERE visible;
CREATE TABLE IF NOT EXISTS favorites.activity_history(id text NOT NULL,revision bigint NOT NULL,day date NOT NULL,data jsonb NOT NULL,recorded_xid xid8 NOT NULL DEFAULT pg_current_xact_id(),PRIMARY KEY(id,revision));
CREATE INDEX IF NOT EXISTS favorites_activity_history_page ON favorites.activity_history(id COLLATE "C",revision DESC);
CREATE INDEX IF NOT EXISTS favorites_activity_history_day ON favorites.activity_history(day,id,revision);
CREATE TABLE IF NOT EXISTS favorites.activity_retention(singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),revision bigint NOT NULL DEFAULT 0);
INSERT INTO favorites.activity_retention(singleton) VALUES(true) ON CONFLICT DO NOTHING;
CREATE OR REPLACE FUNCTION favorites.invalidate_activity() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN UPDATE favorites.activity_retention SET revision=revision+1 WHERE singleton;RETURN NULL;END $$;
REVOKE ALL ON FUNCTION favorites.invalidate_activity() FROM PUBLIC;
-- Transition tables require a separate trigger per event. Empty cleanup leaves cursors valid.
CREATE OR REPLACE FUNCTION favorites.invalidate_changed_activity() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF EXISTS (SELECT FROM changed) THEN
        UPDATE favorites.activity_retention SET revision=revision+1 WHERE singleton;
    END IF;
    RETURN NULL;
END $$;
REVOKE ALL ON FUNCTION favorites.invalidate_changed_activity() FROM PUBLIC;
DROP TRIGGER IF EXISTS invalidate_activity ON favorites.activity_history;
CREATE OR REPLACE TRIGGER invalidate_activity_update AFTER UPDATE ON favorites.activity_history
    REFERENCING OLD TABLE AS changed FOR EACH STATEMENT EXECUTE FUNCTION favorites.invalidate_changed_activity();
CREATE OR REPLACE TRIGGER invalidate_activity_delete AFTER DELETE ON favorites.activity_history
    REFERENCING OLD TABLE AS changed FOR EACH STATEMENT EXECUTE FUNCTION favorites.invalidate_changed_activity();
CREATE OR REPLACE TRIGGER truncate_activity AFTER TRUNCATE ON favorites.activity_history FOR EACH STATEMENT EXECUTE FUNCTION favorites.invalidate_activity();
REVOKE ALL ON TABLE favorites.activity_buckets,favorites.activity_history,favorites.activity_retention FROM PUBLIC;

CREATE TABLE IF NOT EXISTS favorites.outbox(id text PRIMARY KEY,data jsonb NOT NULL,seq bigint GENERATED ALWAYS AS IDENTITY UNIQUE,created_at timestamptz NOT NULL DEFAULT now(),sent_at timestamptz);
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid='favorites.outbox'::regclass AND attname='created_at' AND NOT attisdropped) THEN
  ALTER TABLE favorites.outbox ADD COLUMN created_at timestamptz;
  UPDATE favorites.outbox SET created_at=(data->>'time')::timestamptz;
  ALTER TABLE favorites.outbox ALTER COLUMN created_at SET DEFAULT now(), ALTER COLUMN created_at SET NOT NULL;
 END IF;
END $$;
CREATE INDEX IF NOT EXISTS favorites_outbox_pending_seq ON favorites.outbox(seq) WHERE sent_at IS NULL;
DROP INDEX IF EXISTS favorites.favorites_outbox_pending;
REVOKE ALL ON favorites.outbox FROM PUBLIC;
CREATE INDEX IF NOT EXISTS favorites_outbox_expiry ON favorites.outbox(sent_at,id) WHERE sent_at IS NOT NULL;

-- Each row stores the signed activity transported from one UTC date into the
-- following Beijing date. Both runtime generations keep it current via Outbox.
ALTER TABLE favorites.activity_retention ADD COLUMN IF NOT EXISTS shanghai_day boolean NOT NULL DEFAULT false;
ALTER TABLE favorites.activity_retention ADD COLUMN IF NOT EXISTS calendar_initialized boolean NOT NULL DEFAULT false;
ALTER TABLE favorites.activity_history ADD COLUMN IF NOT EXISTS signed_count bigint;
ALTER TABLE favorites.outbox ADD COLUMN IF NOT EXISTS calendar_conversion boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS favorites.activity_calendar_shifts(
    kind text NOT NULL,target_id text NOT NULL,utc_day date NOT NULL,amount bigint NOT NULL,
    PRIMARY KEY(kind,target_id,utc_day)
);
ALTER TABLE favorites.activity_calendar_shifts ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON TABLE favorites.activity_calendar_shifts FROM PUBLIC;

CREATE OR REPLACE FUNCTION favorites.capture_activity_signed_count() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    IF NEW.signed_count IS NULL THEN
        SELECT first_count INTO STRICT NEW.signed_count
        FROM favorites.activity_buckets WHERE id=NEW.id AND revision=NEW.revision;
    END IF;
    RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION favorites.capture_activity_signed_count() FROM PUBLIC;
CREATE OR REPLACE TRIGGER capture_activity_signed_count BEFORE INSERT ON favorites.activity_history
    FOR EACH ROW EXECUTE FUNCTION favorites.capture_activity_signed_count();

CREATE OR REPLACE FUNCTION favorites.track_activity_calendar() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    is_shanghai boolean;
    event_type text := NEW.data->>'type';
    occurred_at timestamptz;
    event_utc_day date;
    event_beijing_day date;
    bucket record;
    previous_count bigint := 0;
BEGIN
    IF NEW.calendar_conversion OR coalesce(event_type,'') NOT IN ('discovery.activity.v1','discovery.activity.v2') THEN
        RETURN NULL;
    END IF;
    SELECT shanghai_day INTO STRICT is_shanghai FROM favorites.activity_retention WHERE singleton;
    -- PostgreSQL stores microseconds. Truncate sub-microsecond digits so an
    -- old writer's final nanosecond before midnight stays on that date.
    occurred_at := regexp_replace(NEW.data->>'time','([.][0-9]{6})[0-9]+','\1')::timestamptz;
    event_utc_day := (occurred_at AT TIME ZONE 'UTC')::date;
    event_beijing_day := (occurred_at AT TIME ZONE 'Asia/Shanghai')::date;
    SELECT kind,target_id,day,first_count,revision INTO STRICT bucket
    FROM favorites.activity_buckets WHERE id=NEW.data->>'subject';
    IF event_type <> (CASE WHEN is_shanghai THEN 'discovery.activity.v2' ELSE 'discovery.activity.v1' END)
        OR bucket.day IS DISTINCT FROM (CASE WHEN is_shanghai THEN event_beijing_day ELSE event_utc_day END)
        OR bucket.revision IS DISTINCT FROM (NEW.data->'data'->>'revision')::bigint THEN
        RAISE EXCEPTION 'favorites activity event does not match accounting calendar or revision';
    END IF;
    IF event_beijing_day = event_utc_day THEN
        RETURN NULL;
    END IF;
    IF bucket.revision > 1 THEN
        SELECT signed_count INTO STRICT previous_count
        FROM favorites.activity_history WHERE id=NEW.data->>'subject' AND revision=bucket.revision-1;
        IF previous_count IS NULL THEN
            RAISE EXCEPTION 'favorites activity predecessor has no signed count';
        END IF;
    END IF;
    INSERT INTO favorites.activity_calendar_shifts AS shifts(kind,target_id,utc_day,amount)
    VALUES(bucket.kind,bucket.target_id,event_utc_day,bucket.first_count-previous_count)
    ON CONFLICT(kind,target_id,utc_day) DO UPDATE SET amount=shifts.amount+EXCLUDED.amount;
    RETURN NULL;
END $$;
REVOKE ALL ON FUNCTION favorites.track_activity_calendar() FROM PUBLIC;
CREATE OR REPLACE TRIGGER track_activity_calendar AFTER INSERT ON favorites.outbox
    FOR EACH ROW EXECUTE FUNCTION favorites.track_activity_calendar();

-- Deduplicate once, after the activity tables exist. Removing a duplicate has
-- the same accounting day as an ordinary cancellation, including across days.
DO $$
DECLARE
    removed_target record;
    migration_time timestamptz := transaction_timestamp();
    is_shanghai boolean := (SELECT shanghai_day FROM favorites.activity_retention WHERE singleton);
    bucket_day date := (migration_time AT TIME ZONE CASE WHEN is_shanghai THEN 'Asia/Shanghai' ELSE 'UTC' END)::date;
    event_type text := CASE WHEN is_shanghai THEN 'discovery.activity.v2' ELSE 'discovery.activity.v1' END;
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
        INSERT INTO favorites.activity_buckets(id,kind,target_id,day,first_count,revision,visible)
        VALUES(bucket_id,removed_target.kind,removed_target.canonical_target_id,bucket_day,-removed_target.amount,1,false)
        ON CONFLICT(id) DO UPDATE SET first_count=favorites.activity_buckets.first_count+EXCLUDED.first_count,
                                      revision=favorites.activity_buckets.revision+1
        RETURNING greatest(first_count,0),revision,(first_count>0)
        INTO net_count,bucket_revision,bucket_visible;

        snapshot := jsonb_build_object(
            'source','favorites','id',bucket_id,'day',to_char(bucket_day,'YYYY-MM-DD'),
            'target',jsonb_build_object('source','catalog','kind',removed_target.kind,'id',removed_target.canonical_target_id),
            'revision',bucket_revision,'visible',bucket_visible,'metrics',jsonb_build_object('favorites',net_count));
        UPDATE favorites.activity_buckets SET visible=bucket_visible,data=snapshot WHERE id=bucket_id;
        INSERT INTO favorites.activity_history(id,revision,day,data) VALUES(bucket_id,bucket_revision,bucket_day,snapshot);
        event_id := 'activity:' || encode(sha256(convert_to(bucket_id,'UTF8')),'hex') || ':' || bucket_revision;
        INSERT INTO favorites.outbox(id,data,created_at,calendar_conversion) VALUES(event_id,jsonb_build_object(
            'specversion','1.0','id',event_id,'source','favorites','type',event_type,
            'subject',bucket_id,'time',migration_time,'data',snapshot),migration_time,
            NOT (SELECT calendar_initialized FROM favorites.activity_retention WHERE singleton));
        PERFORM pg_notify('favorites_outbox','');
    END LOOP;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS favorites_items_canonical ON favorites.items(owner_key,kind,canonical_target_id) WHERE canonical_target_id<>'';

-- Final completion can occur later than this module's own closure step.
ALTER TABLE favorites.owners ADD COLUMN IF NOT EXISTS closure_completed_at timestamptz;
CREATE INDEX IF NOT EXISTS favorites_closure_marker_expiry ON favorites.owners(closure_completed_at,owner_key) WHERE closure_completed_at IS NOT NULL;
CREATE OR REPLACE FUNCTION favorites.mark_closure_completed(p_owner text,p_id text,p_at timestamptz)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF p_at IS NULL OR NOT isfinite(p_at) OR p_at>statement_timestamp()+interval '5 minutes' THEN
    RAISE EXCEPTION 'invalid closure completion' USING ERRCODE='22023';
 END IF;
 UPDATE favorites.owners SET closure_completed_at=p_at WHERE owner_key=p_owner AND closure_id=p_id AND closure_completed_at IS NULL;
 IF EXISTS (SELECT 1 FROM favorites.owners WHERE owner_key=p_owner AND (closure_id IS DISTINCT FROM p_id OR closure_completed_at IS DISTINCT FROM p_at)) THEN
    RAISE EXCEPTION 'inconsistent closure completion' USING ERRCODE='22023';
 END IF;
END;
$$;
CREATE OR REPLACE FUNCTION favorites.prune_closure_markers(p_limit integer) RETURNS bigint
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog SET TimeZone='Asia/Shanghai' AS $$
 WITH due AS (SELECT owner_key FROM favorites.owners WHERE closure_completed_at + interval '12 months' <= statement_timestamp()
   ORDER BY closure_completed_at,owner_key LIMIT least(greatest(p_limit,0),1000) FOR UPDATE SKIP LOCKED),
 removed AS (DELETE FROM favorites.owners t USING due WHERE t.owner_key=due.owner_key RETURNING 1)
 SELECT count(*) FROM removed
$$;
REVOKE ALL ON FUNCTION favorites.mark_closure_completed(text,text,timestamptz),favorites.prune_closure_markers(integer) FROM PUBLIC;
