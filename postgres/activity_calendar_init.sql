-- Restore known signed deltas where a retained envelope supplies the exact
-- instant. History and Outbox both retain public values, which clamp negatives.
WITH envelopes AS MATERIALIZED (
    SELECT data->>'subject' AS id,(data->'data'->>'revision')::bigint AS revision,
        (data->'data'->'metrics'->>'favorites')::bigint AS published,
        regexp_replace(data->>'time','([.][0-9]{6})[0-9]+','\1')::timestamptz AS at
    FROM favorites.outbox WHERE data->>'type'='discovery.activity.v1'
), revisions AS (
    SELECT id,revision,(data->'metrics'->>'favorites')::bigint AS published
    FROM favorites.activity_history
    UNION
    SELECT id,revision,published FROM envelopes
), adjacent AS (
    SELECT id,revision,published,
        lag(revision,1,0) OVER ordered AS previous_revision,
        lag(published,1,0) OVER ordered AS previous_published
    FROM revisions
    WINDOW ordered AS (PARTITION BY id ORDER BY revision)
), uniquely_timed AS (
    SELECT id,at,min(revision) AS revision
    FROM envelopes GROUP BY id,at HAVING count(*)=1
), witnessed AS (
    -- A still-present item and its uniquely timed activity envelope identify
    -- a contribution even if both sides of the revision were clamped at zero.
    SELECT e.id,e.revision
    FROM uniquely_timed e
    JOIN favorites.activity_buckets b ON b.id=e.id
    JOIN favorites.items i ON i.kind=b.kind AND i.canonical_target_id=b.target_id AND i.created_at=e.at
    GROUP BY e.id,e.revision HAVING count(*)=1
), pure_times AS (
    -- If revision equals net, each mutation was a unit addition. Surviving
    -- items supply dates even after their envelopes have expired. Account for
    -- envelope/item overlap as a multiset at each stored timestamp.
    SELECT b.id,e.at,count(*) AS amount
    FROM favorites.activity_buckets b JOIN envelopes e ON e.id=b.id
    WHERE b.first_count=b.revision AND (e.at AT TIME ZONE 'UTC')::date=b.day
    GROUP BY b.id,e.at
    UNION ALL
    SELECT b.id,i.created_at,count(*)
    FROM favorites.activity_buckets b
    JOIN favorites.items i ON i.kind=b.kind AND i.canonical_target_id=b.target_id
        AND (i.created_at AT TIME ZONE 'UTC')::date=b.day
    WHERE b.first_count=b.revision
    GROUP BY b.id,i.created_at
), pure_known AS (
    SELECT b.id,sum(t.amount) AS known_net,
        sum(t.amount) FILTER (WHERE (t.at AT TIME ZONE 'Asia/Shanghai')::date=b.day+1) AS known_shift
    FROM (SELECT id,at,max(amount) AS amount FROM pure_times GROUP BY id,at) t
    JOIN favorites.activity_buckets b USING(id)
    GROUP BY b.id,b.first_count HAVING sum(t.amount)<=b.first_count
), known AS (
    SELECT a.id,
        (e.at AT TIME ZONE 'Asia/Shanghai')::date=b.day+1 AS next_day,
        CASE
            WHEN a.published=1 AND a.previous_published=0 THEN 1
            WHEN w.id IS NOT NULL AND a.published=0 AND a.previous_published=0 THEN 1
            WHEN (a.published>0 OR a.revision=b.revision)
                AND (a.previous_published>0 OR a.previous_revision=0)
            THEN CASE WHEN a.revision=b.revision THEN b.first_count ELSE a.published END-a.previous_published
        END AS delta
    FROM adjacent a
    JOIN envelopes e USING(id,revision)
    JOIN favorites.activity_buckets b ON b.id=a.id
    LEFT JOIN witnessed w ON w.id=a.id AND w.revision=a.revision
    WHERE a.previous_revision=a.revision-1 AND (e.at AT TIME ZONE 'UTC')::date=b.day
), known_by_bucket AS (
    SELECT id,sum(delta) AS known_net,sum(delta) FILTER (WHERE next_day) AS known_shift
    FROM known WHERE id NOT IN (SELECT id FROM pure_known)
    GROUP BY id
    UNION ALL
    SELECT id,known_net,known_shift FROM pure_known
), elapsed AS (
    SELECT b.*, b.day::timestamp AT TIME ZONE 'UTC' AS started_at,
        least((b.day+1)::timestamp AT TIME ZONE 'UTC',$1::timestamptz) AS elapsed_until,
        (b.day+1)::timestamp AT TIME ZONE 'Asia/Shanghai' AS next_beijing_day
    FROM favorites.activity_buckets b
), shift AS (
    SELECT b.kind,b.target_id,b.day,
        coalesce(k.known_shift,0) + CASE WHEN b.elapsed_until>b.started_at THEN round(
            (b.first_count-coalesce(k.known_net,0))
            * greatest(0,extract(epoch FROM b.elapsed_until-b.next_beijing_day))
            / extract(epoch FROM b.elapsed_until-b.started_at)
        ) ELSE 0 END AS amount
    FROM elapsed b LEFT JOIN known_by_bucket k USING(id)
)
INSERT INTO favorites.activity_calendar_shifts(kind,target_id,utc_day,amount)
SELECT kind,target_id,day,amount FROM shift WHERE amount<>0;
