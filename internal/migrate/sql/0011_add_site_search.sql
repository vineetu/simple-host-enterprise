BEGIN;

CREATE TABLE site_search_documents (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    site_id        uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    version_number integer NOT NULL,
    owner_name     text NOT NULL,
    site_name      text NOT NULL,
    page_path      text NOT NULL,
    url_path       text NOT NULL,
    title          text NOT NULL,
    description    text NOT NULL,
    headings       text NOT NULL,
    body_text      text NOT NULL,
    indexed_at     timestamptz NOT NULL,
    search_vector  tsvector GENERATED ALWAYS AS (
        setweight(
            to_tsvector(
                'english'::regconfig,
                coalesce(owner_name, '') || ' ' ||
                coalesce(site_name, '') || ' ' ||
                coalesce(title, '')
            ),
            'A'
        ) ||
        setweight(
            to_tsvector(
                'english'::regconfig,
                coalesce(description, '') || ' ' ||
                coalesce(headings, '')
            ),
            'B'
        ) ||
        setweight(
            to_tsvector('english'::regconfig, coalesce(body_text, '')),
            'D'
        )
    ) STORED,
    CONSTRAINT site_search_documents_site_version_page_key
        UNIQUE (site_id, version_number, page_path),
    CONSTRAINT site_search_documents_version_positive
        CHECK (version_number > 0)
);

CREATE INDEX site_search_documents_search_vector_idx
    ON site_search_documents USING GIN (search_vector);

CREATE INDEX site_search_documents_site_version_idx
    ON site_search_documents (site_id, version_number);

-- This queue deliberately has no foreign key to sites. A delete tombstone must
-- outlive the authoritative site row for a future external search backend.
CREATE TABLE site_search_queue (
    site_id      uuid PRIMARY KEY,
    operation    text NOT NULL,
    generation   bigint NOT NULL,
    lease_token  text,
    available_at timestamptz NOT NULL,
    locked_until timestamptz,
    attempts     integer NOT NULL,
    last_error   text,
    updated_at   timestamptz NOT NULL,
    CONSTRAINT site_search_queue_operation_check
        CHECK (operation IN ('reconcile', 'delete')),
    CONSTRAINT site_search_queue_generation_positive
        CHECK (generation > 0),
    CONSTRAINT site_search_queue_attempts_nonnegative
        CHECK (attempts >= 0),
    CONSTRAINT site_search_queue_lease_pair_check
        CHECK (
            (lease_token IS NULL AND locked_until IS NULL) OR
            (lease_token IS NOT NULL AND locked_until IS NOT NULL)
        )
);

CREATE INDEX site_search_queue_due_idx
    ON site_search_queue (available_at, updated_at, site_id);

CREATE TABLE site_search_index_status (
    site_id           uuid PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
    version_number    integer NOT NULL,
    extractor_version integer NOT NULL,
    document_count    integer NOT NULL,
    partial           boolean NOT NULL,
    indexed_at        timestamptz NOT NULL,
    CONSTRAINT site_search_index_status_version_positive
        CHECK (version_number > 0),
    CONSTRAINT site_search_index_status_extractor_positive
        CHECK (extractor_version > 0),
    CONSTRAINT site_search_index_status_document_count_nonnegative
        CHECK (document_count >= 0)
);

CREATE TABLE site_search_queries (
    id               uuid PRIMARY KEY,
    normalized_query text NOT NULL,
    result_count     integer NOT NULL,
    session_digest   text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT site_search_queries_result_count_nonnegative
        CHECK (result_count >= 0)
);

CREATE INDEX site_search_queries_created_at_idx
    ON site_search_queries (created_at);

CREATE TABLE site_search_impressions (
    id              uuid PRIMARY KEY,
    query_id        uuid NOT NULL REFERENCES site_search_queries(id) ON DELETE CASCADE,
    result_position integer NOT NULL,
    site_id         uuid NOT NULL,
    version_number  integer NOT NULL,
    page_path       text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT site_search_impressions_position_positive
        CHECK (result_position > 0),
    CONSTRAINT site_search_impressions_version_positive
        CHECK (version_number > 0)
);

CREATE INDEX site_search_impressions_query_id_idx
    ON site_search_impressions (query_id);

CREATE INDEX site_search_impressions_created_at_idx
    ON site_search_impressions (created_at);

CREATE TABLE site_search_clicks (
    impression_id  uuid NOT NULL REFERENCES site_search_impressions(id) ON DELETE CASCADE,
    session_digest text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT site_search_clicks_impression_session_key
        PRIMARY KEY (impression_id, session_digest)
);

CREATE INDEX site_search_clicks_created_at_idx
    ON site_search_clicks (created_at);

INSERT INTO site_search_queue (
    site_id,
    operation,
    generation,
    lease_token,
    available_at,
    locked_until,
    attempts,
    last_error,
    updated_at
)
SELECT
    id,
    'reconcile',
    1,
    NULL,
    now(),
    NULL,
    0,
    NULL,
    now()
FROM sites
ON CONFLICT (site_id) DO NOTHING;

COMMIT;
