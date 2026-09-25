package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type healthResponse struct {
	Status string `json:"status"`
}

// requiredSchemaProbe validates the runtime schema required by migrations 0009
// through 0012 and 0019, including the keys and indexes used by runtime writes.
//
// It is a last resort, not a rollout step: with one replica and no automatic
// restore, a binary that never becomes Ready is an outage. The rollout runs
// this SQL by hand against production before the binary goes out. Migrations
// 0013 to 0018 are still unprobed, which is a known gap and a separate change.
//
// expected_team_triggers no longer lists users_teams_have_no_key: migration
// 0025 drops that trigger (and the users.api_key column it guarded) outright,
// one-way, per design.md 9.3/10.2. Probing for a trigger a later migration in
// this same chain deliberately removes made every binary built after 0025
// permanently unready. The other two 0019 triggers still apply unchanged.
const requiredSchemaProbe = `
	WITH expected_site_columns (column_name, udt_name, column_default) AS (
		VALUES
			('uses_state', 'bool', 'false'),
			('uses_versioned_state', 'bool', 'false')
	),
	expected_download_columns (column_name, udt_name) AS (
		VALUES
			('site_id', 'uuid'),
			('path', 'text'),
			('day', 'date'),
			('downloads', 'int8'),
			('first_seen_at', 'timestamptz'),
			('last_seen_at', 'timestamptz')
	),
	expected_collaboration_columns (table_name, column_name, udt_name, is_nullable, column_default) AS (
		VALUES
			('site_collaborators', 'site_id', 'uuid', 'NO', NULL),
			('site_collaborators', 'user_id', 'uuid', 'NO', NULL),
			('site_collaborators', 'role', 'text', 'NO', '''editor''::text'),
			('site_collaborators', 'added_by', 'uuid', 'YES', NULL),
			('site_collaborators', 'created_at', 'timestamptz', 'NO', 'now()'),
			('versions', 'uploaded_by', 'uuid', 'YES', NULL)
	),
	expected_collaboration_foreign_keys (table_name, key_columns, target_table, target_columns, delete_action) AS (
		VALUES
			('site_collaborators', ARRAY['site_id']::text[], 'sites', ARRAY['id']::text[], 'c'),
			('site_collaborators', ARRAY['user_id']::text[], 'users', ARRAY['id']::text[], 'c'),
			('site_collaborators', ARRAY['added_by']::text[], 'users', ARRAY['id']::text[], 'n'),
			('versions', ARRAY['uploaded_by']::text[], 'users', ARRAY['id']::text[], 'n')
	),
	expected_search_columns (table_name, column_name, udt_name, is_nullable) AS (
		VALUES
			('site_search_documents', 'id', 'int8', 'NO'),
			('site_search_documents', 'site_id', 'uuid', 'NO'),
			('site_search_documents', 'version_number', 'int4', 'NO'),
			('site_search_documents', 'owner_name', 'text', 'NO'),
			('site_search_documents', 'site_name', 'text', 'NO'),
			('site_search_documents', 'page_path', 'text', 'NO'),
			('site_search_documents', 'url_path', 'text', 'NO'),
			('site_search_documents', 'title', 'text', 'NO'),
			('site_search_documents', 'description', 'text', 'NO'),
			('site_search_documents', 'headings', 'text', 'NO'),
			('site_search_documents', 'body_text', 'text', 'NO'),
			('site_search_documents', 'indexed_at', 'timestamptz', 'NO'),
			('site_search_documents', 'search_vector', 'tsvector', 'YES'),
			('site_search_queue', 'site_id', 'uuid', 'NO'),
			('site_search_queue', 'operation', 'text', 'NO'),
			('site_search_queue', 'generation', 'int8', 'NO'),
			('site_search_queue', 'lease_token', 'text', 'YES'),
			('site_search_queue', 'available_at', 'timestamptz', 'NO'),
			('site_search_queue', 'locked_until', 'timestamptz', 'YES'),
			('site_search_queue', 'attempts', 'int4', 'NO'),
			('site_search_queue', 'last_error', 'text', 'YES'),
			('site_search_queue', 'updated_at', 'timestamptz', 'NO'),
			('site_search_index_status', 'site_id', 'uuid', 'NO'),
			('site_search_index_status', 'version_number', 'int4', 'NO'),
			('site_search_index_status', 'extractor_version', 'int4', 'NO'),
			('site_search_index_status', 'document_count', 'int4', 'NO'),
			('site_search_index_status', 'partial', 'bool', 'NO'),
			('site_search_index_status', 'indexed_at', 'timestamptz', 'NO'),
			('site_search_queries', 'id', 'uuid', 'NO'),
			('site_search_queries', 'normalized_query', 'text', 'NO'),
			('site_search_queries', 'result_count', 'int4', 'NO'),
			('site_search_queries', 'session_digest', 'text', 'NO'),
			('site_search_queries', 'created_at', 'timestamptz', 'NO'),
			('site_search_impressions', 'id', 'uuid', 'NO'),
			('site_search_impressions', 'query_id', 'uuid', 'NO'),
			('site_search_impressions', 'result_position', 'int4', 'NO'),
			('site_search_impressions', 'site_id', 'uuid', 'NO'),
			('site_search_impressions', 'version_number', 'int4', 'NO'),
			('site_search_impressions', 'page_path', 'text', 'NO'),
			('site_search_impressions', 'created_at', 'timestamptz', 'NO'),
			('site_search_clicks', 'impression_id', 'uuid', 'NO'),
			('site_search_clicks', 'session_digest', 'text', 'NO'),
			('site_search_clicks', 'created_at', 'timestamptz', 'NO')
	),
	expected_search_keys (table_name, key_columns) AS (
		VALUES
			('site_search_documents', ARRAY['id']::text[]),
			('site_search_documents', ARRAY['site_id', 'version_number', 'page_path']::text[]),
			('site_search_queue', ARRAY['site_id']::text[]),
			('site_search_index_status', ARRAY['site_id']::text[]),
			('site_search_queries', ARRAY['id']::text[]),
			('site_search_impressions', ARRAY['id']::text[]),
			('site_search_clicks', ARRAY['impression_id', 'session_digest']::text[])
	),
	expected_search_indexes (table_name, access_method, key_columns) AS (
		VALUES
			('site_search_documents', 'btree', ARRAY['site_id', 'version_number']::text[]),
			('site_search_queue', 'btree', ARRAY['available_at', 'updated_at', 'site_id']::text[]),
			('site_search_queries', 'btree', ARRAY['created_at']::text[]),
			('site_search_impressions', 'btree', ARRAY['query_id']::text[]),
			('site_search_impressions', 'btree', ARRAY['created_at']::text[]),
			('site_search_clicks', 'btree', ARRAY['created_at']::text[])
	),
	expected_search_foreign_keys (table_name, key_columns, target_table, target_columns) AS (
		VALUES
			('site_search_documents', ARRAY['site_id']::text[], 'sites', ARRAY['id']::text[]),
			('site_search_index_status', ARRAY['site_id']::text[], 'sites', ARRAY['id']::text[]),
			('site_search_impressions', ARRAY['query_id']::text[], 'site_search_queries', ARRAY['id']::text[]),
			('site_search_clicks', ARRAY['impression_id']::text[], 'site_search_impressions', ARRAY['id']::text[])
	),
	expected_team_columns (table_name, column_name, udt_name, is_nullable) AS (
		VALUES
			('users', 'kind', 'text', 'NO'),
			('users', 'email', 'text', 'YES'),
			('users', 'email_source', 'text', 'YES'),
			('team_members', 'team_id', 'uuid', 'NO'),
			('team_members', 'user_id', 'uuid', 'NO'),
			('team_members', 'added_by', 'uuid', 'YES'),
			('team_members', 'created_at', 'timestamptz', 'NO'),
			('team_audit', 'id', 'uuid', 'NO'),
			('team_audit', 'team_id', 'uuid', 'NO'),
			('team_audit', 'actor_id', 'uuid', 'YES'),
			('team_audit', 'action', 'text', 'NO'),
			('team_audit', 'subject_id', 'uuid', 'YES'),
			('team_audit', 'detail', 'text', 'YES'),
			('team_audit', 'created_at', 'timestamptz', 'NO')
	),
	expected_team_triggers (table_name, trigger_name) AS (
		VALUES
			('users', 'users_kind_locked'),
			('team_members', 'team_members_kinds')
	),
	site_columns_ready AS (
		SELECT count(*) = (SELECT count(*) FROM expected_site_columns) AS ready
		FROM expected_site_columns AS expected
		JOIN information_schema.columns AS actual
			ON actual.table_schema = 'public'
			AND actual.table_name = 'sites'
			AND actual.column_name = expected.column_name
			AND actual.udt_name = expected.udt_name
			AND actual.is_nullable = 'NO'
			AND actual.column_default = expected.column_default
	),
	download_columns_ready AS (
		SELECT count(*) = (SELECT count(*) FROM expected_download_columns) AS ready
		FROM expected_download_columns AS expected
		JOIN information_schema.columns AS actual
			ON actual.table_schema = 'public'
			AND actual.table_name = 'site_file_downloads'
			AND actual.column_name = expected.column_name
			AND actual.udt_name = expected.udt_name
			AND actual.is_nullable = 'NO'
	),
	download_index_ready AS (
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_index AS index_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = index_catalog.indrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			WHERE table_namespace.nspname = 'public'
				AND table_catalog.relname = 'site_file_downloads'
				AND index_catalog.indisvalid
				AND index_catalog.indisready
				AND (index_catalog.indisunique OR index_catalog.indisprimary)
				AND index_catalog.indpred IS NULL
				AND index_catalog.indnkeyatts = 3
				AND (
					SELECT pg_catalog.array_agg(
						attribute_catalog.attname::text
						ORDER BY key_column.ordinality
					)
					FROM unnest(index_catalog.indkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = index_catalog.indrelid
						AND attribute_catalog.attnum = key_column.attnum
					WHERE key_column.ordinality <= index_catalog.indnkeyatts
				) = ARRAY['site_id', 'path', 'day']::text[]
		) AS ready
	),
	collaboration_columns_ready AS (
		SELECT count(*) = (SELECT count(*) FROM expected_collaboration_columns) AS ready
		FROM expected_collaboration_columns AS expected
		JOIN information_schema.columns AS actual
			ON actual.table_schema = 'public'
			AND actual.table_name = expected.table_name
			AND actual.column_name = expected.column_name
			AND actual.udt_name = expected.udt_name
			AND actual.is_nullable = expected.is_nullable
			AND actual.column_default IS NOT DISTINCT FROM expected.column_default
	),
	collaboration_primary_key_ready AS (
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_constraint AS constraint_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = constraint_catalog.conrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			WHERE constraint_catalog.contype = 'p'
				AND table_namespace.nspname = 'public'
				AND table_catalog.relname = 'site_collaborators'
				AND (
					SELECT pg_catalog.array_agg(
						attribute_catalog.attname::text
						ORDER BY key_column.ordinality
					)
					FROM unnest(constraint_catalog.conkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = constraint_catalog.conrelid
						AND attribute_catalog.attnum = key_column.attnum
				) = ARRAY['site_id', 'user_id']::text[]
		) AS ready
	),
	collaboration_reverse_index_ready AS (
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_index AS index_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = index_catalog.indrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			JOIN pg_catalog.pg_class AS index_relation
				ON index_relation.oid = index_catalog.indexrelid
			JOIN pg_catalog.pg_am AS access_method
				ON access_method.oid = index_relation.relam
			WHERE table_namespace.nspname = 'public'
				AND table_catalog.relname = 'site_collaborators'
				AND access_method.amname = 'btree'
				AND index_catalog.indisvalid
				AND index_catalog.indisready
				AND NOT index_catalog.indisunique
				AND NOT index_catalog.indisprimary
				AND index_catalog.indpred IS NULL
				AND index_catalog.indnatts = 2
				AND index_catalog.indnkeyatts = 2
				AND (
					SELECT pg_catalog.array_agg(
						attribute_catalog.attname::text
						ORDER BY key_column.ordinality
					)
					FROM unnest(index_catalog.indkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = index_catalog.indrelid
						AND attribute_catalog.attnum = key_column.attnum
					WHERE key_column.ordinality <= index_catalog.indnkeyatts
				) = ARRAY['user_id', 'site_id']::text[]
		) AS ready
	),
	collaboration_foreign_keys_ready AS (
		SELECT count(*) = (SELECT count(*) FROM expected_collaboration_foreign_keys) AS ready
		FROM expected_collaboration_foreign_keys AS expected
		WHERE EXISTS (
			SELECT 1
			FROM pg_catalog.pg_constraint AS constraint_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = constraint_catalog.conrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			JOIN pg_catalog.pg_class AS target_catalog
				ON target_catalog.oid = constraint_catalog.confrelid
			JOIN pg_catalog.pg_namespace AS target_namespace
				ON target_namespace.oid = target_catalog.relnamespace
			WHERE constraint_catalog.contype = 'f'
				AND constraint_catalog.convalidated
				AND constraint_catalog.confdeltype = expected.delete_action::"char"
				AND table_namespace.nspname = 'public'
				AND target_namespace.nspname = 'public'
				AND table_catalog.relname = expected.table_name
				AND target_catalog.relname = expected.target_table
				AND (
					SELECT pg_catalog.array_agg(attribute_catalog.attname::text ORDER BY key_column.ordinality)
					FROM unnest(constraint_catalog.conkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = constraint_catalog.conrelid
						AND attribute_catalog.attnum = key_column.attnum
				) = expected.key_columns
				AND (
					SELECT pg_catalog.array_agg(attribute_catalog.attname::text ORDER BY key_column.ordinality)
					FROM unnest(constraint_catalog.confkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = constraint_catalog.confrelid
						AND attribute_catalog.attnum = key_column.attnum
				) = expected.target_columns
		)
	),
	collaboration_role_check_ready AS (
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_constraint AS constraint_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = constraint_catalog.conrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			WHERE constraint_catalog.contype = 'c'
				AND constraint_catalog.convalidated
				AND table_namespace.nspname = 'public'
				AND table_catalog.relname = 'site_collaborators'
				AND pg_catalog.regexp_replace(
					pg_catalog.pg_get_expr(constraint_catalog.conbin, constraint_catalog.conrelid),
					'[[:space:]()]', '', 'g'
				) = 'role=''editor''::text'
		) AS ready
	),
	search_columns_ready AS (
		SELECT count(*) = (SELECT count(*) FROM expected_search_columns) AS ready
		FROM expected_search_columns AS expected
		JOIN information_schema.columns AS actual
			ON actual.table_schema = 'public'
			AND actual.table_name = expected.table_name
			AND actual.column_name = expected.column_name
			AND actual.udt_name = expected.udt_name
			AND actual.is_nullable = expected.is_nullable
	),
	search_vector_ready AS (
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_attribute AS attribute_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = attribute_catalog.attrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			JOIN pg_catalog.pg_attrdef AS default_catalog
				ON default_catalog.adrelid = attribute_catalog.attrelid
				AND default_catalog.adnum = attribute_catalog.attnum
			WHERE table_namespace.nspname = 'public'
				AND table_catalog.relname = 'site_search_documents'
				AND attribute_catalog.attname = 'search_vector'
				AND attribute_catalog.atttypid = 'pg_catalog.tsvector'::pg_catalog.regtype
				AND attribute_catalog.attgenerated = 's'
				AND pg_catalog.pg_get_expr(default_catalog.adbin, default_catalog.adrelid)
					LIKE '%''english''::regconfig%'
				AND pg_catalog.pg_get_expr(default_catalog.adbin, default_catalog.adrelid)
					LIKE '%setweight%'
				AND pg_catalog.pg_get_expr(default_catalog.adbin, default_catalog.adrelid)
					LIKE '%owner_name%'
				AND pg_catalog.pg_get_expr(default_catalog.adbin, default_catalog.adrelid)
					LIKE '%site_name%'
				AND pg_catalog.pg_get_expr(default_catalog.adbin, default_catalog.adrelid)
					LIKE '%title%'
				AND pg_catalog.pg_get_expr(default_catalog.adbin, default_catalog.adrelid)
					LIKE '%description%'
				AND pg_catalog.pg_get_expr(default_catalog.adbin, default_catalog.adrelid)
					LIKE '%headings%'
				AND pg_catalog.pg_get_expr(default_catalog.adbin, default_catalog.adrelid)
					LIKE '%body_text%'
		) AS ready
	),
	search_vector_index_ready AS (
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_index AS index_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = index_catalog.indrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			JOIN pg_catalog.pg_class AS index_relation
				ON index_relation.oid = index_catalog.indexrelid
			JOIN pg_catalog.pg_am AS access_method
				ON access_method.oid = index_relation.relam
			WHERE table_namespace.nspname = 'public'
				AND table_catalog.relname = 'site_search_documents'
				AND access_method.amname = 'gin'
				AND index_catalog.indisvalid
				AND index_catalog.indisready
				AND index_catalog.indpred IS NULL
				AND index_catalog.indnkeyatts = 1
				AND (
					SELECT pg_catalog.array_agg(
						attribute_catalog.attname::text
						ORDER BY key_column.ordinality
					)
					FROM unnest(index_catalog.indkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = index_catalog.indrelid
						AND attribute_catalog.attnum = key_column.attnum
					WHERE key_column.ordinality <= index_catalog.indnkeyatts
				) = ARRAY['search_vector']::text[]
		) AS ready
	),
	search_keys_ready AS (
		SELECT count(*) = (SELECT count(*) FROM expected_search_keys) AS ready
		FROM expected_search_keys AS expected
		WHERE EXISTS (
			SELECT 1
			FROM pg_catalog.pg_index AS index_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = index_catalog.indrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			WHERE table_namespace.nspname = 'public'
				AND table_catalog.relname = expected.table_name
				AND index_catalog.indisvalid
				AND index_catalog.indisready
				AND (index_catalog.indisunique OR index_catalog.indisprimary)
				AND index_catalog.indpred IS NULL
				AND index_catalog.indnkeyatts = pg_catalog.cardinality(expected.key_columns)
				AND (
					SELECT pg_catalog.array_agg(
						attribute_catalog.attname::text
						ORDER BY key_column.ordinality
					)
					FROM unnest(index_catalog.indkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = index_catalog.indrelid
						AND attribute_catalog.attnum = key_column.attnum
					WHERE key_column.ordinality <= index_catalog.indnkeyatts
				) = expected.key_columns
		)
	),
	search_indexes_ready AS (
		SELECT count(*) = (SELECT count(*) FROM expected_search_indexes) AS ready
		FROM expected_search_indexes AS expected
		WHERE EXISTS (
			SELECT 1
			FROM pg_catalog.pg_index AS index_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = index_catalog.indrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			JOIN pg_catalog.pg_class AS index_relation
				ON index_relation.oid = index_catalog.indexrelid
			JOIN pg_catalog.pg_am AS access_method
				ON access_method.oid = index_relation.relam
			WHERE table_namespace.nspname = 'public'
				AND table_catalog.relname = expected.table_name
				AND access_method.amname = expected.access_method
				AND index_catalog.indisvalid
				AND index_catalog.indisready
				AND index_catalog.indpred IS NULL
				AND index_catalog.indnkeyatts = pg_catalog.cardinality(expected.key_columns)
				AND (
					SELECT pg_catalog.array_agg(
						attribute_catalog.attname::text
						ORDER BY key_column.ordinality
					)
					FROM unnest(index_catalog.indkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = index_catalog.indrelid
						AND attribute_catalog.attnum = key_column.attnum
					WHERE key_column.ordinality <= index_catalog.indnkeyatts
				) = expected.key_columns
		)
	),
	search_foreign_keys_ready AS (
		SELECT count(*) = (SELECT count(*) FROM expected_search_foreign_keys) AS ready
		FROM expected_search_foreign_keys AS expected
		WHERE EXISTS (
			SELECT 1
			FROM pg_catalog.pg_constraint AS constraint_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = constraint_catalog.conrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			JOIN pg_catalog.pg_class AS target_catalog
				ON target_catalog.oid = constraint_catalog.confrelid
			JOIN pg_catalog.pg_namespace AS target_namespace
				ON target_namespace.oid = target_catalog.relnamespace
			WHERE constraint_catalog.contype = 'f'
				AND constraint_catalog.confdeltype = 'c'
				AND table_namespace.nspname = 'public'
				AND target_namespace.nspname = 'public'
				AND table_catalog.relname = expected.table_name
				AND target_catalog.relname = expected.target_table
				AND (
					SELECT pg_catalog.array_agg(
						attribute_catalog.attname::text
						ORDER BY key_column.ordinality
					)
					FROM unnest(constraint_catalog.conkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = constraint_catalog.conrelid
						AND attribute_catalog.attnum = key_column.attnum
				) = expected.key_columns
				AND (
					SELECT pg_catalog.array_agg(
						attribute_catalog.attname::text
						ORDER BY key_column.ordinality
					)
					FROM unnest(constraint_catalog.confkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = constraint_catalog.confrelid
						AND attribute_catalog.attnum = key_column.attnum
				) = expected.target_columns
		)
	),
	search_queue_has_no_foreign_key AS (
		SELECT NOT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_constraint AS constraint_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = constraint_catalog.conrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			WHERE constraint_catalog.contype = 'f'
				AND table_namespace.nspname = 'public'
				AND table_catalog.relname = 'site_search_queue'
		) AS ready
	),
	search_queue_operation_ready AS (
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_constraint AS constraint_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = constraint_catalog.conrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			WHERE constraint_catalog.contype = 'c'
				AND table_namespace.nspname = 'public'
				AND table_catalog.relname = 'site_search_queue'
				AND pg_catalog.pg_get_constraintdef(constraint_catalog.oid) LIKE '%operation%'
				AND pg_catalog.pg_get_constraintdef(constraint_catalog.oid) LIKE '%reconcile%'
				AND pg_catalog.pg_get_constraintdef(constraint_catalog.oid) LIKE '%delete%'
		) AS ready
	),
	team_columns_ready AS (
		SELECT count(*) = (SELECT count(*) FROM expected_team_columns) AS ready
		FROM expected_team_columns AS expected
		JOIN information_schema.columns AS actual
			ON actual.table_schema = 'public'
			AND actual.table_name = expected.table_name
			AND actual.column_name = expected.column_name
			AND actual.udt_name = expected.udt_name
			AND actual.is_nullable = expected.is_nullable
	),
	team_kind_check_ready AS (
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_constraint AS constraint_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = constraint_catalog.conrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			WHERE constraint_catalog.contype = 'c'
				AND table_namespace.nspname = 'public'
				AND table_catalog.relname = 'users'
				AND constraint_catalog.conname = 'users_kind_check'
		) AS ready
	),
	team_members_primary_key_ready AS (
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_constraint AS constraint_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = constraint_catalog.conrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			WHERE constraint_catalog.contype = 'p'
				AND table_namespace.nspname = 'public'
				AND table_catalog.relname = 'team_members'
				AND (
					SELECT pg_catalog.array_agg(
						attribute_catalog.attname::text
						ORDER BY key_column.ordinality
					)
					FROM unnest(constraint_catalog.conkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = constraint_catalog.conrelid
						AND attribute_catalog.attnum = key_column.attnum
				) = ARRAY['team_id', 'user_id']::text[]
		) AS ready
	),
	team_members_reverse_index_ready AS (
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_index AS index_catalog
			JOIN pg_catalog.pg_class AS table_catalog
				ON table_catalog.oid = index_catalog.indrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
				ON table_namespace.oid = table_catalog.relnamespace
			WHERE table_namespace.nspname = 'public'
				AND table_catalog.relname = 'team_members'
				AND index_catalog.indisvalid
				AND index_catalog.indisready
				AND NOT index_catalog.indisprimary
				AND index_catalog.indpred IS NULL
				AND index_catalog.indnkeyatts = 2
				AND (
					SELECT pg_catalog.array_agg(
						attribute_catalog.attname::text
						ORDER BY key_column.ordinality
					)
					FROM unnest(index_catalog.indkey) WITH ORDINALITY AS key_column (attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_catalog
						ON attribute_catalog.attrelid = index_catalog.indrelid
						AND attribute_catalog.attnum = key_column.attnum
					WHERE key_column.ordinality <= index_catalog.indnkeyatts
				) = ARRAY['user_id', 'team_id']::text[]
		) AS ready
	),
	-- Present is not enough: a disabled trigger (tgenabled = 'D') still shows
	-- in the catalogue and enforces nothing, and these three are the guards
	-- that stop a team holding a key or a membership row outliving its kinds.
	team_triggers_ready AS (
		SELECT count(*) = (SELECT count(*) FROM expected_team_triggers) AS ready
		FROM expected_team_triggers AS expected
		JOIN pg_catalog.pg_trigger AS trigger_catalog
			ON trigger_catalog.tgname = expected.trigger_name
			AND NOT trigger_catalog.tgisinternal
			AND trigger_catalog.tgenabled <> 'D'
		JOIN pg_catalog.pg_class AS table_catalog
			ON table_catalog.oid = trigger_catalog.tgrelid
			AND table_catalog.relname = expected.table_name
		JOIN pg_catalog.pg_namespace AS table_namespace
			ON table_namespace.oid = table_catalog.relnamespace
			AND table_namespace.nspname = 'public'
	)
	SELECT
		site_columns_ready.ready
		AND download_columns_ready.ready
		AND download_index_ready.ready
		AND collaboration_columns_ready.ready
		AND collaboration_primary_key_ready.ready
		AND collaboration_reverse_index_ready.ready
		AND collaboration_foreign_keys_ready.ready
		AND collaboration_role_check_ready.ready
		AND search_columns_ready.ready
		AND search_vector_ready.ready
		AND search_vector_index_ready.ready
		AND search_keys_ready.ready
		AND search_indexes_ready.ready
		AND search_foreign_keys_ready.ready
		AND search_queue_has_no_foreign_key.ready
		AND search_queue_operation_ready.ready
		AND team_columns_ready.ready
		AND team_kind_check_ready.ready
		AND team_members_primary_key_ready.ready
		AND team_members_reverse_index_ready.ready
		AND team_triggers_ready.ready
	FROM
		site_columns_ready,
		download_columns_ready,
		download_index_ready,
		collaboration_columns_ready,
		collaboration_primary_key_ready,
		collaboration_reverse_index_ready,
		collaboration_foreign_keys_ready,
		collaboration_role_check_ready,
		search_columns_ready,
		search_vector_ready,
		search_vector_index_ready,
		search_keys_ready,
		search_indexes_ready,
		search_foreign_keys_ready,
		search_queue_has_no_foreign_key,
		search_queue_operation_ready,
		team_columns_ready,
		team_kind_check_ready,
		team_members_primary_key_ready,
		team_members_reverse_index_ready,
		team_triggers_ready
`

// RegisterHealthRoutes mounts the probes. pingBucket is the site store's
// bucket check: a replica that cannot reach the bucket cannot deploy or fill
// its cache, so it takes itself out of rotation.
func RegisterHealthRoutes(mux *http.ServeMux, db *sql.DB, pingBucket func(context.Context) error) {
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyz(db, pingBucket))
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func readyz(db *sql.DB, pingBucket func(context.Context) error) http.HandlerFunc {
	return readinessHandler(
		func(ctx context.Context) error {
			if err := db.PingContext(ctx); err != nil {
				return err
			}
			if pingBucket == nil {
				return nil
			}
			return pingBucket(ctx)
		},
		func(ctx context.Context) error {
			var schemaReady bool
			if err := db.QueryRowContext(ctx, requiredSchemaProbe).Scan(&schemaReady); err != nil {
				return err
			}
			return requireSchemaReady(schemaReady)
		},
	)
}

func requireSchemaReady(ready bool) error {
	if !ready {
		return errors.New("required database schema is not ready")
	}
	return nil
}

func readinessHandler(
	ping func(context.Context) error,
	checkSchema func(context.Context) error,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "unready"})
			return
		}
		if err := checkSchema(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "unready"})
			return
		}

		writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
