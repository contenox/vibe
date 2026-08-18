package runtimetypes_test

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

var (
	createTableRe  = regexp.MustCompile(`(?is)^CREATE\s+(VIRTUAL\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*(?:USING\s+[A-Za-z0-9_]+\s*)?\(`)
	addColumnRe    = regexp.MustCompile(`(?is)^ALTER\s+TABLE\s+([A-Za-z_][A-Za-z0-9_]*)\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)
	renameTableRe  = regexp.MustCompile(`(?is)^ALTER\s+TABLE\s+([A-Za-z_][A-Za-z0-9_]*)\s+RENAME\s+TO\s+([A-Za-z_][A-Za-z0-9_]*)`)
	dropTableRe    = regexp.MustCompile(`(?is)^DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)
	createIndexRe  = regexp.MustCompile(`(?is)^CREATE\s+(UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)\s+ON\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?:USING\s+([A-Za-z0-9_]+)\s*)?\(`)
	dropIndexRe    = regexp.MustCompile(`(?is)^DROP\s+INDEX\s+(?:IF\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)
	tableOptionRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*=`)
	leadingIdentRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)`)
	whitespaceRe   = regexp.MustCompile(`\s+`)
)

var tableConstraintKeywords = map[string]bool{
	"PRIMARY": true, "UNIQUE": true, "FOREIGN": true, "CHECK": true,
	"CONSTRAINT": true, "EXCLUDE": true,
}

var columnClauseRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^NOT\s+NULL\b`),
	regexp.MustCompile(`(?i)^PRIMARY\s+KEY\b`),
	regexp.MustCompile(`(?i)^AUTOINCREMENT\b`),
	regexp.MustCompile(`(?i)^REFERENCES\b`),
	regexp.MustCompile(`(?i)^DEFAULT\b`),
	regexp.MustCompile(`(?i)^UNIQUE\b`),
	regexp.MustCompile(`(?i)^CHECK\b`),
	regexp.MustCompile(`(?i)^COLLATE\b`),
	regexp.MustCompile(`(?i)^NULL\b`),
}

var equivalentTypes = map[string][]string{
	"BLOB":                  {"BYTEA"},
	"INTEGER":               {"BIGINT"},
	"INTEGER AUTOINCREMENT": {"BIGSERIAL"},
}

var equivalentDefaults = map[string]string{
	"unixepoch('now')": "EXTRACT(EPOCH FROM now())::bigint",
}

var columnDetailExemptTables = map[string]string{
	"workspace_chunks_fts": "SQLite declares it with CREATE VIRTUAL TABLE ... USING fts5, whose column list carries no types, nullability or defaults to compare",
}

var postgresOnlyIndexes = map[string]string{
	"idx_workspace_chunks_fts_text":   "the SQLite side of workspace_chunks_fts is an FTS5 virtual table, which maintains its own inverted index; Postgres needs an explicit GIN index to answer the same lexical narrowing",
	"idx_workspace_chunks_fts_chunk":  "FTS5 answers chunk_id lookups from the virtual table itself; the Postgres mirror is a plain table and needs the b-tree",
	"idx_workspace_chunks_fts_config": "FTS5 answers config_id lookups from the virtual table itself; the Postgres mirror is a plain table and needs the b-tree",
}

type columnShape struct {
	Type       string
	NotNull    bool
	Default    string
	PrimaryKey bool
	Unique     bool
	Check      string
	References string
}

type tableShape struct {
	Virtual     bool
	Columns     map[string]columnShape
	Constraints []string
}

type indexShape struct {
	Table  string
	Unique bool
	Method string
	Body   string
	Where  string
}

type schemaShape struct {
	Tables    map[string]*tableShape
	Indexes   map[string]indexShape
	Conflicts []string
}

func TestUnit_Schema_Parity_SQLiteAndPostgresDeclareTheSameTables(t *testing.T) {
	sqlite := parseSchemaShape(t, runtimetypes.SchemaSQLite)
	postgres := parseSchemaShape(t, runtimetypes.SchemaPostgres)

	requireParserSane(t, sqlite, "schema_sqlite.sql")
	requireParserSane(t, postgres, "schema_postgres.sql")

	problems := compareSchemaShapes(sqlite, postgres)
	require.Empty(t, problems, "the two schemas have drifted apart:\n%s", strings.Join(problems, "\n"))
}

func TestUnit_Schema_Parity_CatchesEveryKindOfDrift(t *testing.T) {
	for name, tc := range map[string]struct {
		dialect string
		from    string
		to      string
		expect  string
	}{
		"dropped_not_null": {
			dialect: "sqlite",
			from:    "    payload TEXT NOT NULL,\n    added_at TIMESTAMP NOT NULL,",
			to:      "    payload TEXT,\n    added_at TIMESTAMP NOT NULL,",
			expect:  "payload",
		},
		"changed_default": {
			dialect: "sqlite",
			from:    "on_timeout   VARCHAR(20) NOT NULL DEFAULT 'deny'",
			to:      "on_timeout   VARCHAR(20) NOT NULL DEFAULT 'allow'",
			expect:  "on_timeout",
		},
		"dropped_default": {
			dialect: "postgres",
			from:    "args_summary TEXT NOT NULL DEFAULT ''",
			to:      "args_summary TEXT NOT NULL",
			expect:  "args_summary",
		},
		"widened_type": {
			dialect: "sqlite",
			from:    "tools_name   VARCHAR(255) NOT NULL",
			to:      "tools_name   VARCHAR(512) NOT NULL",
			expect:  "VARCHAR(512)",
		},
		"unmapped_type": {
			dialect: "postgres",
			from:    "state        VARCHAR(20) NOT NULL DEFAULT 'pending'",
			to:      "state        JSONB NOT NULL DEFAULT 'pending'",
			expect:  "JSONB",
		},
		"unmapped_blob_type": {
			dialect: "postgres",
			from:    "vector       BYTEA NOT NULL",
			to:      "vector       TEXT NOT NULL",
			expect:  "vector",
		},
		"dropped_primary_key_column": {
			dialect: "sqlite",
			from:    "    PRIMARY KEY (key, workspace_id)",
			to:      "    PRIMARY KEY (key)",
			expect:  "kv",
		},
		"dropped_column_unique": {
			dialect: "sqlite",
			from:    "    name VARCHAR(512) NOT NULL UNIQUE,\n    purpose_type",
			to:      "    name VARCHAR(512) NOT NULL,\n    purpose_type",
			expect:  "name",
		},
		"dropped_table_unique": {
			dialect: "postgres",
			from:    "    UNIQUE(type, base_url)",
			to:      "    CHECK(type <> '')",
			expect:  "llm_backends",
		},
		"dropped_references": {
			dialect: "sqlite",
			from:    "idx_id VARCHAR(255) NOT NULL REFERENCES message_indices(id) ON DELETE CASCADE",
			to:      "idx_id VARCHAR(255) NOT NULL",
			expect:  "idx_id",
		},
		"dropped_check": {
			dialect: "postgres",
			from:    "    id       INTEGER PRIMARY KEY CHECK (id = 1)",
			to:      "    id       INTEGER PRIMARY KEY",
			expect:  "event_nid_seq",
		},
		"dropped_index": {
			dialect: "sqlite",
			from:    "CREATE INDEX IF NOT EXISTS idx_agents_kind ON agents(kind);",
			to:      "",
			expect:  "idx_agents_kind",
		},
		"changed_index_columns": {
			dialect: "sqlite",
			from:    "ON hitl_approvals(state, created_at)",
			to:      "ON hitl_approvals(created_at)",
			expect:  "idx_hitl_approvals_state_created",
		},
		"changed_index_table": {
			dialect: "postgres",
			from:    "idx_messages_added_at ON messages (added_at)",
			to:      "idx_messages_added_at ON message_indices (added_at)",
			expect:  "idx_messages_added_at",
		},
		"changed_index_method": {
			dialect: "postgres",
			from:    "CREATE INDEX IF NOT EXISTS idx_agents_kind ON agents(kind);",
			to:      "CREATE INDEX IF NOT EXISTS idx_agents_kind ON agents USING GIN(kind);",
			expect:  "idx_agents_kind",
		},
		"index_became_unique": {
			dialect: "postgres",
			from:    "CREATE INDEX IF NOT EXISTS idx_agents_kind",
			to:      "CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_kind",
			expect:  "idx_agents_kind",
		},
		"dropped_partial_index_predicate": {
			dialect: "sqlite",
			from:    "    ON message_indices (name, workspace_id)\n    WHERE name IS NOT NULL",
			to:      "    ON message_indices (name, workspace_id)",
			expect:  "idx_message_indices_name",
		},
		"weakened_added_column": {
			dialect: "postgres",
			from:    "ALTER TABLE remote_tools ADD COLUMN IF NOT EXISTS inject_params_json TEXT NOT NULL DEFAULT '{}';",
			to:      "ALTER TABLE remote_tools ADD COLUMN IF NOT EXISTS inject_params_json TEXT;",
			expect:  "inject_params_json",
		},
		"dropped_table": {
			dialect: "sqlite",
			from:    "CREATE TABLE IF NOT EXISTS local_fs_reads",
			to:      "CREATE TABLE IF NOT EXISTS local_fs_read",
			expect:  "local_fs_reads",
		},
		"dropped_fts_column": {
			dialect: "postgres",
			from:    "    chunk_id  VARCHAR(255) NOT NULL,\n",
			to:      "",
			expect:  "chunk_id",
		},
		"stale_column_detail_exemption": {
			dialect: "sqlite",
			from:    "CREATE VIRTUAL TABLE IF NOT EXISTS workspace_chunks_fts USING fts5(",
			to:      "CREATE TABLE IF NOT EXISTS workspace_chunks_fts (",
			expect:  "exemption",
		},
		"stale_postgres_only_index_allowance": {
			dialect: "sqlite",
			from:    "CREATE INDEX IF NOT EXISTS idx_workspace_chunks_config_path\n    ON workspace_chunks(config_id, path);",
			to:      "CREATE INDEX IF NOT EXISTS idx_workspace_chunks_config_path\n    ON workspace_chunks(config_id, path);\nCREATE INDEX IF NOT EXISTS idx_workspace_chunks_fts_chunk ON workspace_chunks_fts(chunk_id);",
			expect:  "allowance",
		},
	} {
		t.Run(name, func(t *testing.T) {
			sqlite, postgres := runtimetypes.SchemaSQLite, runtimetypes.SchemaPostgres
			switch tc.dialect {
			case "sqlite":
				sqlite = mutateSchema(t, sqlite, tc.from, tc.to)
			case "postgres":
				postgres = mutateSchema(t, postgres, tc.from, tc.to)
			default:
				t.Fatalf("unknown dialect %q", tc.dialect)
			}

			problems := compareSchemaShapes(parseSchemaShape(t, sqlite), parseSchemaShape(t, postgres))
			require.NotEmpty(t, problems, "this drift was not caught")
			require.Contains(t, strings.Join(problems, "\n"), tc.expect)
		})
	}
}

func mutateSchema(t *testing.T, schema, from, to string) string {
	t.Helper()
	require.Equal(t, 1, strings.Count(schema, from),
		"the drift case anchors on %q, which must appear in the schema exactly once", from)
	return strings.Replace(schema, from, to, 1)
}

func compareSchemaShapes(sqlite, postgres *schemaShape) []string {
	var problems []string

	for _, name := range sortedKeys(sqlite.Tables) {
		if _, ok := postgres.Tables[name]; !ok {
			problems = append(problems, fmt.Sprintf("table %q: in schema_sqlite.sql, missing from schema_postgres.sql", name))
		}
	}
	for _, name := range sortedKeys(postgres.Tables) {
		if _, ok := sqlite.Tables[name]; !ok {
			problems = append(problems, fmt.Sprintf("table %q: in schema_postgres.sql, missing from schema_sqlite.sql", name))
		}
	}

	for _, name := range sortedKeys(sqlite.Tables) {
		pgTable, ok := postgres.Tables[name]
		if !ok {
			continue
		}
		problems = append(problems, compareTables(name, sqlite.Tables[name], pgTable)...)
	}

	return append(problems, compareIndexes(sqlite, postgres)...)
}

func compareTables(name string, sqlite, postgres *tableShape) []string {
	var problems []string

	for _, col := range sortedKeys(sqlite.Columns) {
		if _, ok := postgres.Columns[col]; !ok {
			problems = append(problems, fmt.Sprintf("table %q column %q: in schema_sqlite.sql, missing from schema_postgres.sql", name, col))
		}
	}
	for _, col := range sortedKeys(postgres.Columns) {
		if _, ok := sqlite.Columns[col]; !ok {
			problems = append(problems, fmt.Sprintf("table %q column %q: in schema_postgres.sql, missing from schema_sqlite.sql", name, col))
		}
	}

	if reason, exempt := columnDetailExemptTables[name]; exempt {
		if !sqlite.Virtual {
			problems = append(problems, fmt.Sprintf(
				"table %q: its column details are exempt from comparison because %s, but schema_sqlite.sql no longer declares it as a virtual table; drop the exemption and compare it like every other table",
				name, reason))
		}
	} else {
		for _, col := range sortedKeys(sqlite.Columns) {
			pgCol, ok := postgres.Columns[col]
			if !ok {
				continue
			}
			problems = append(problems, compareColumns(name, col, sqlite.Columns[col], pgCol)...)
		}
	}

	if lite, pg := sortedCopy(sqlite.Constraints), sortedCopy(postgres.Constraints); !equalStrings(lite, pg) {
		problems = append(problems, fmt.Sprintf(
			"table %q table constraints differ: schema_sqlite.sql has [%s], schema_postgres.sql has [%s]",
			name, strings.Join(lite, "; "), strings.Join(pg, "; ")))
	}

	return problems
}

func compareColumns(table, name string, sqlite, postgres columnShape) []string {
	var problems []string

	if !equivalentType(sqlite.Type, postgres.Type) {
		problems = append(problems, fmt.Sprintf(
			"table %q column %q type differs: schema_sqlite.sql says %s, schema_postgres.sql says %s. Dialect spellings of one type belong in equivalentTypes; anything else is drift",
			table, name, sqlite.Type, postgres.Type))
	}
	if sqlite.NotNull != postgres.NotNull {
		problems = append(problems, fmt.Sprintf("table %q column %q nullability differs: schema_sqlite.sql %s, schema_postgres.sql %s",
			table, name, nullability(sqlite.NotNull), nullability(postgres.NotNull)))
	}
	if !equivalentDefault(sqlite, postgres) {
		problems = append(problems, fmt.Sprintf(
			"table %q column %q default differs: schema_sqlite.sql says %s, schema_postgres.sql says %s. Two spellings of one value belong in equivalentDefaults; anything else is drift",
			table, name, quoteOrNone(sqlite.Default), quoteOrNone(postgres.Default)))
	}
	if sqlite.PrimaryKey != postgres.PrimaryKey {
		problems = append(problems, fmt.Sprintf("table %q column %q PRIMARY KEY membership differs: schema_sqlite.sql %t, schema_postgres.sql %t",
			table, name, sqlite.PrimaryKey, postgres.PrimaryKey))
	}
	if sqlite.Unique != postgres.Unique {
		problems = append(problems, fmt.Sprintf("table %q column %q UNIQUE differs: schema_sqlite.sql %t, schema_postgres.sql %t",
			table, name, sqlite.Unique, postgres.Unique))
	}
	if sqlite.Check != postgres.Check {
		problems = append(problems, fmt.Sprintf("table %q column %q CHECK differs: schema_sqlite.sql says %s, schema_postgres.sql says %s",
			table, name, quoteOrNone(sqlite.Check), quoteOrNone(postgres.Check)))
	}
	if sqlite.References != postgres.References {
		problems = append(problems, fmt.Sprintf("table %q column %q foreign key differs: schema_sqlite.sql says %s, schema_postgres.sql says %s",
			table, name, quoteOrNone(sqlite.References), quoteOrNone(postgres.References)))
	}

	return problems
}

func compareIndexes(sqlite, postgres *schemaShape) []string {
	var problems []string

	for _, name := range sortedKeys(sqlite.Indexes) {
		if reason, allowed := postgresOnlyIndexes[name]; allowed {
			problems = append(problems, fmt.Sprintf(
				"index %q is allowed to exist only in schema_postgres.sql because %s, but schema_sqlite.sql now declares it too; drop the allowance",
				name, reason))
			continue
		}
		if _, ok := postgres.Indexes[name]; !ok {
			problems = append(problems, fmt.Sprintf("index %q: in schema_sqlite.sql, missing from schema_postgres.sql", name))
		}
	}
	for _, name := range sortedKeys(postgres.Indexes) {
		if _, ok := sqlite.Indexes[name]; ok {
			continue
		}
		if _, allowed := postgresOnlyIndexes[name]; allowed {
			continue
		}
		problems = append(problems, fmt.Sprintf("index %q: in schema_postgres.sql, missing from schema_sqlite.sql", name))
	}

	for _, name := range sortedKeys(sqlite.Indexes) {
		pgIndex, ok := postgres.Indexes[name]
		if !ok {
			continue
		}
		lite := sqlite.Indexes[name]
		if lite.Table != pgIndex.Table {
			problems = append(problems, fmt.Sprintf("index %q covers %q in schema_sqlite.sql and %q in schema_postgres.sql",
				name, lite.Table, pgIndex.Table))
		}
		if lite.Unique != pgIndex.Unique {
			problems = append(problems, fmt.Sprintf("index %q uniqueness differs: schema_sqlite.sql %t, schema_postgres.sql %t",
				name, lite.Unique, pgIndex.Unique))
		}
		if lite.Method != pgIndex.Method {
			problems = append(problems, fmt.Sprintf("index %q method differs: schema_sqlite.sql %s, schema_postgres.sql %s",
				name, quoteOrNone(lite.Method), quoteOrNone(pgIndex.Method)))
		}
		if lite.Body != pgIndex.Body {
			problems = append(problems, fmt.Sprintf("index %q covers (%s) in schema_sqlite.sql and (%s) in schema_postgres.sql",
				name, lite.Body, pgIndex.Body))
		}
		if lite.Where != pgIndex.Where {
			problems = append(problems, fmt.Sprintf("index %q predicate differs: schema_sqlite.sql says %s, schema_postgres.sql says %s",
				name, quoteOrNone(lite.Where), quoteOrNone(pgIndex.Where)))
		}
	}

	return problems
}

func equivalentType(sqlite, postgres string) bool {
	if sqlite == postgres {
		return true
	}
	for _, alias := range equivalentTypes[sqlite] {
		if alias == postgres {
			return true
		}
	}
	return false
}

func equivalentDefault(sqlite, postgres columnShape) bool {
	lite, pg := sqlite.Default, postgres.Default
	if strings.HasPrefix(sqlite.Type, "BOOLEAN") {
		lite, pg = booleanDefault(lite), booleanDefault(pg)
	}
	if lite == pg {
		return true
	}
	alias, mapped := equivalentDefaults[lite]
	return mapped && alias == pg
}

func booleanDefault(value string) string {
	switch strings.ToUpper(value) {
	case "0", "FALSE":
		return "FALSE"
	case "1", "TRUE":
		return "TRUE"
	}
	return value
}

func nullability(notNull bool) string {
	if notNull {
		return "NOT NULL"
	}
	return "nullable"
}

func quoteOrNone(value string) string {
	if value == "" {
		return "none"
	}
	return `"` + value + `"`
}

func requireParserSane(t *testing.T, shape *schemaShape, file string) {
	t.Helper()

	require.GreaterOrEqual(t, len(shape.Tables), 25, "%s: parsed too few tables; the parser, not the schema, is probably broken", file)
	require.GreaterOrEqual(t, len(shape.Indexes), 22, "%s: parsed too few indexes; the parser, not the schema, is probably broken", file)
	require.NotContains(t, shape.Tables, "llm_backends_temp", "%s: scratch tables must not survive the walk", file)
	require.Empty(t, shape.Conflicts, "%s: a fresh install and an upgraded database would not get the same column:\n%s",
		file, strings.Join(shape.Conflicts, "\n"))

	backends := requireTable(t, shape, file, "llm_backends")
	require.Equal(t, "VARCHAR(255)", backends.Columns["id"].Type)
	require.True(t, backends.Columns["id"].PrimaryKey)
	require.True(t, backends.Columns["name"].NotNull)
	require.True(t, backends.Columns["name"].Unique)
	require.Contains(t, backends.Columns, "base_url")
	require.Contains(t, backends.Constraints, "UNIQUE(TYPE,BASE_URL)", "%s: table constraints must be parsed, not skipped", file)

	indices := requireTable(t, shape, file, "message_indices")
	require.Contains(t, indices.Columns, "agent_id", "%s: columns introduced by ALTER TABLE must be picked up", file)
	require.False(t, indices.Columns["agent_id"].NotNull)
	require.Empty(t, indices.Columns["agent_id"].Default)
	require.Equal(t, "''", indices.Columns["workspace_id"].Default)

	mcp := requireTable(t, shape, file, "mcp_servers")
	require.True(t, mcp.Columns["headers_json"].NotNull, "%s: an ALTER-added column keeps its NOT NULL", file)
	require.Equal(t, "'{}'", mcp.Columns["headers_json"].Default)
	require.Equal(t, "'sse'", mcp.Columns["transport"].Default)

	hitl := requireTable(t, shape, file, "hitl_approvals")
	require.Contains(t, hitl.Columns, "mission_id")
	require.False(t, hitl.Columns["mission_id"].NotNull)
	require.Equal(t, "'pending'", hitl.Columns["state"].Default)
	require.Equal(t, "", hitl.Columns["resolution"].Default)

	seq := requireTable(t, shape, file, "event_nid_seq")
	require.NotContains(t, seq.Columns, "tokenize")
	require.Contains(t, seq.Columns, "id")
	require.Contains(t, seq.Columns, "last_nid")
	require.Len(t, seq.Columns, 2)
	require.Contains(t, seq.Columns["id"].Check, "ID = 1", "%s: a column CHECK must be parsed", file)

	fts := requireTable(t, shape, file, "workspace_chunks_fts")
	require.Contains(t, fts.Columns, "chunk_id")
	require.Len(t, fts.Columns, 3)

	assignments := requireTable(t, shape, file, "llm_affinity_group_backend_assignments")
	require.Contains(t, assignments.Columns["group_id"].References, "ON DELETE CASCADE",
		"%s: a column's foreign key must be parsed", file)

	partial, ok := shape.Indexes["idx_message_indices_name"]
	require.True(t, ok, "%s: partial indexes must be picked up", file)
	require.True(t, partial.Unique)
	require.Equal(t, "message_indices", partial.Table)
	require.Contains(t, partial.Where, "NAME IS NOT NULL")

	firings, ok := shape.Indexes["idx_event_firings_ws_nid"]
	require.True(t, ok, "%s: index bodies must keep their column order", file)
	require.Equal(t, "WORKSPACE_ID,NID DESC,TRIGGER_NAME,STATUS", firings.Body)
	require.False(t, firings.Unique)
	require.Empty(t, firings.Where)
}

func requireTable(t *testing.T, shape *schemaShape, file, name string) *tableShape {
	t.Helper()
	table, ok := shape.Tables[name]
	require.True(t, ok, "%s: expected table %q", file, name)
	return table
}

func parseSchemaShape(t *testing.T, schema string) *schemaShape {
	t.Helper()

	shape := &schemaShape{Tables: map[string]*tableShape{}, Indexes: map[string]indexShape{}}
	for _, stmt := range splitSchemaStatements(stripSchemaComments(schema)) {
		switch {
		case createTableRe.MatchString(stmt):
			loc := createTableRe.FindStringSubmatchIndex(stmt)
			name := stmt[loc[4]:loc[5]]
			body, _, ok := parenSpan(stmt, loc[1]-1)
			require.True(t, ok, "unbalanced parentheses in: %s", stmt)

			table := shape.Tables[name]
			if table == nil {
				table = &tableShape{Columns: map[string]columnShape{}}
				shape.Tables[name] = table
			}
			table.Virtual = loc[2] >= 0
			for _, entry := range splitTopLevel(body) {
				entry = strings.TrimSpace(entry)
				if entry == "" || tableOptionRe.MatchString(entry) {
					continue
				}
				ident := leadingIdentRe.FindString(entry)
				if ident == "" {
					continue
				}
				if tableConstraintKeywords[strings.ToUpper(ident)] {
					table.Constraints = append(table.Constraints, normalizeSQL(entry))
					continue
				}
				table.Columns[strings.ToLower(ident)] = parseColumnShape(entry[len(ident):])
			}
		case addColumnRe.MatchString(stmt):
			loc := addColumnRe.FindStringSubmatchIndex(stmt)
			name, column := stmt[loc[2]:loc[3]], strings.ToLower(stmt[loc[4]:loc[5]])
			table := shape.Tables[name]
			if table == nil {
				table = &tableShape{Columns: map[string]columnShape{}}
				shape.Tables[name] = table
			}
			added := parseColumnShape(stmt[loc[5]:])
			if existing, ok := table.Columns[column]; ok && existing != added {
				shape.Conflicts = append(shape.Conflicts, fmt.Sprintf(
					"table %q column %q: CREATE TABLE declares %+v, the ALTER TABLE below it declares %+v",
					name, column, existing, added))
			}
			table.Columns[column] = added
		case renameTableRe.MatchString(stmt):
			m := renameTableRe.FindStringSubmatch(stmt)
			if table, ok := shape.Tables[m[1]]; ok {
				shape.Tables[m[2]] = table
				delete(shape.Tables, m[1])
			}
		case dropTableRe.MatchString(stmt):
			delete(shape.Tables, dropTableRe.FindStringSubmatch(stmt)[1])
		case createIndexRe.MatchString(stmt):
			loc := createIndexRe.FindStringSubmatchIndex(stmt)
			body, end, ok := parenSpan(stmt, loc[1]-1)
			require.True(t, ok, "unbalanced parentheses in: %s", stmt)
			index := indexShape{
				Table:  stmt[loc[6]:loc[7]],
				Unique: loc[2] >= 0,
				Body:   normalizeSQL(body),
			}
			if loc[8] >= 0 {
				index.Method = strings.ToUpper(stmt[loc[8]:loc[9]])
			}
			if tail := strings.TrimSpace(stmt[end+1:]); tail != "" {
				index.Where = normalizeSQL(strings.TrimPrefix(normalizeSQL(tail), "WHERE "))
			}
			shape.Indexes[stmt[loc[4]:loc[5]]] = index
		case dropIndexRe.MatchString(stmt):
			delete(shape.Indexes, dropIndexRe.FindStringSubmatch(stmt)[1])
		}
	}
	return shape
}

func parseColumnShape(def string) columnShape {
	starts := clauseStarts(def)
	typeEnd := len(def)
	if len(starts) > 0 {
		typeEnd = starts[0]
	}

	shape := columnShape{Type: normalizeSQL(def[:typeEnd])}
	autoIncrement := false
	for i, start := range starts {
		end := len(def)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		clause := strings.TrimSpace(def[start:end])
		switch keyword := strings.ToUpper(leadingIdentRe.FindString(clause)); keyword {
		case "NOT":
			shape.NotNull = true
		case "PRIMARY":
			shape.PrimaryKey = true
		case "UNIQUE":
			shape.Unique = true
		case "AUTOINCREMENT":
			autoIncrement = true
		case "DEFAULT":
			shape.Default = trimOuterParens(collapseSpaces(clause[len(keyword):]))
		case "REFERENCES":
			shape.References = normalizeSQL(clause)
		case "CHECK":
			shape.Check = normalizeSQL(clause)
		}
	}
	if autoIncrement {
		shape.Type += " AUTOINCREMENT"
	}
	return shape
}

func clauseStarts(def string) []int {
	var starts []int
	depth, inString := 0, false
	for i := 0; i < len(def); i++ {
		switch c := def[i]; {
		case c == '\'':
			inString = !inString
			continue
		case inString:
			continue
		case c == '(':
			depth++
			continue
		case c == ')':
			depth--
			continue
		}
		if depth != 0 || (i > 0 && isWordByte(def[i-1])) {
			continue
		}
		for _, re := range columnClauseRes {
			match := re.FindString(def[i:])
			if match == "" {
				continue
			}
			starts = append(starts, i)
			i += len(match) - 1
			break
		}
	}
	return starts
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func collapseSpaces(sql string) string {
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(sql, " "))
}

func trimOuterParens(value string) string {
	for strings.HasPrefix(value, "(") {
		body, end, ok := parenSpan(value, 0)
		if !ok || end != len(value)-1 {
			break
		}
		value = strings.TrimSpace(body)
	}
	return value
}

func normalizeSQL(sql string) string {
	var out strings.Builder
	inString := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if c == '\'' {
			inString = !inString
			out.WriteByte(c)
			continue
		}
		if inString {
			out.WriteByte(c)
			continue
		}
		out.WriteByte(byte(strings.ToUpper(string(c))[0]))
	}
	collapsed := collapseSpaces(out.String())
	for _, around := range []string{"(", ")", ","} {
		collapsed = strings.ReplaceAll(collapsed, " "+around, around)
		collapsed = strings.ReplaceAll(collapsed, around+" ", around)
	}
	return collapsed
}

func parenSpan(stmt string, open int) (string, int, bool) {
	if open < 0 || open >= len(stmt) || stmt[open] != '(' {
		return "", 0, false
	}
	depth, inString := 0, false
	for i := open; i < len(stmt); i++ {
		switch c := stmt[i]; {
		case c == '\'':
			inString = !inString
		case inString:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return stmt[open+1 : i], i, true
			}
		}
	}
	return "", 0, false
}

func splitTopLevel(body string) []string {
	var parts []string
	var cur strings.Builder
	depth, inString := 0, false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == '\'':
			inString = !inString
		case inString:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	return append(parts, cur.String())
}

func splitSchemaStatements(schema string) []string {
	var stmts []string
	var cur strings.Builder
	inString := false
	for i := 0; i < len(schema); i++ {
		c := schema[i]
		if c == '\'' {
			inString = !inString
		}
		if c == ';' && !inString {
			if stmt := strings.TrimSpace(cur.String()); stmt != "" {
				stmts = append(stmts, stmt)
			}
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if stmt := strings.TrimSpace(cur.String()); stmt != "" {
		stmts = append(stmts, stmt)
	}
	return stmts
}

func stripSchemaComments(schema string) string {
	var out strings.Builder
	inString, inComment := false, false
	for i := 0; i < len(schema); i++ {
		c := schema[i]
		if inComment {
			if c == '\n' {
				inComment = false
				out.WriteByte(c)
			}
			continue
		}
		if !inString && c == '-' && i+1 < len(schema) && schema[i+1] == '-' {
			inComment = true
			i++
			continue
		}
		if c == '\'' {
			inString = !inString
		}
		out.WriteByte(c)
	}
	return out.String()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
