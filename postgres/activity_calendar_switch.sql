-- Forward moves the ledger amount to the following date. Rollback applies the
-- opposite impact against the current buckets, including post-cutover writes.
WITH impacts AS (
    SELECT kind,target_id,utc_day AS day,-amount*$1::bigint AS delta FROM favorites.activity_calendar_shifts
    UNION ALL
    SELECT kind,target_id,utc_day+1 AS day,amount*$1::bigint AS delta FROM favorites.activity_calendar_shifts
), combined AS (
    SELECT kind,target_id,day,sum(delta) AS delta FROM impacts
    GROUP BY kind,target_id,day HAVING sum(delta)<>0
), affected AS (
    SELECT kind,target_id,day,delta,
        kind || '/' || translate(encode(convert_to(target_id,'UTF8'),'base64'), '+/=' || chr(10),'-_') ||
        '/' || to_char(day,'YYYY-MM-DD') AS id
    FROM combined
), counters AS (
    SELECT a.id,a.kind,a.target_id,a.day,a.delta+coalesce(b.first_count,0) AS signed_count,
        coalesce(b.revision,0)+1 AS revision
    FROM affected a LEFT JOIN favorites.activity_buckets b USING(id)
), snapshots AS (
    SELECT *,
        jsonb_build_object('source','favorites','id',id,'day',to_char(day,'YYYY-MM-DD'),
            'target',jsonb_build_object('source','catalog','kind',kind,'id',target_id),
            'revision',revision,'visible',signed_count>0,
            'metrics',jsonb_build_object('favorites',greatest(signed_count,0))) AS data
    FROM counters
), saved AS (
    INSERT INTO favorites.activity_buckets(id,kind,target_id,day,first_count,revision,visible,data)
    SELECT id,kind,target_id,day,signed_count,revision,signed_count>0,data FROM snapshots
    ON CONFLICT(id) DO UPDATE SET first_count=EXCLUDED.first_count,revision=EXCLUDED.revision,
        visible=EXCLUDED.visible,data=EXCLUDED.data
    RETURNING id,revision,day,first_count,data
), history AS (
    INSERT INTO favorites.activity_history(id,revision,day,signed_count,data)
    SELECT id,revision,day,first_count,data FROM saved
    RETURNING id,revision,data
), events AS (
    SELECT id,data,'activity:' || encode(sha256(convert_to(id,'UTF8')),'hex') || ':' || revision AS event_id
    FROM history
)
INSERT INTO favorites.outbox(id,data,created_at,calendar_conversion)
SELECT event_id,jsonb_build_object('specversion','1.0','id',event_id,'source','favorites',
    'type',$2::text,'subject',id,'time',transaction_timestamp(),'data',data),transaction_timestamp(),true
FROM events ORDER BY id COLLATE "C";
