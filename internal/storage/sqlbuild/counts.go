package sqlbuild

import "fmt"

// ReadyWorkIssueColumns is IssueSelectColumns qualified with the "i." alias
// used by the counts mega-query.
var ReadyWorkIssueColumns = QualifyColumns(IssueSelectColumns, "i.")

// DepJSONObject renders one dependency row as JSON for JSON_ARRAYAGG
// aggregation in the counts mega-query. Field names must match the JSON tags
// of types.Dependency.
const DepJSONObject = `JSON_OBJECT(
	'issue_id', issue_id,
	'depends_on_id', COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external),
	'type', type,
	'created_at', DATE_FORMAT(created_at, '%Y-%m-%dT%H:%i:%sZ'),
	'created_by', created_by,
	'metadata', CAST(metadata AS CHAR),
	'thread_id', thread_id
)`

// SearchCountsSQL renders the counts mega-query: full issue rows aliased "i"
// plus labels JSON, dep/rdep/comment counts, parent ID, and dependency JSON,
// for one table family. whereSQL/orderBySQL/limitSQL may be empty; the
// reverse-blocker count unions wisp_dependencies only when the caller has
// probed that the table exists.
//
// The scan side is issueops.ScanReadyWorkRowWithCounts, which scans
// IssueSelectColumns positionally followed by the six extra columns in the
// order projected here.
func SearchCountsSQL(tables FilterTables, whereSQL, orderBySQL, limitSQL string, includeWispReverseDeps, skipLabels bool) string {
	reverseBlockerSelect := `
				SELECT COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) AS dep_id
				FROM dependencies WHERE type = 'blocks'
	`
	if includeWispReverseDeps {
		reverseBlockerSelect += `
				UNION ALL
				SELECT COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) AS dep_id
				FROM wisp_dependencies WHERE type = 'blocks'
		`
	}

	labelsSelect := "l.labels_json AS labels_json"
	labelsJoin := fmt.Sprintf(`
		LEFT JOIN (
			SELECT issue_id, JSON_ARRAYAGG(label) AS labels_json
			FROM %s
			WHERE issue_id IN (SELECT id FROM base)
			GROUP BY issue_id
		) l ON l.issue_id = i.id`, tables.Labels)
	if skipLabels {
		labelsSelect = "NULL AS labels_json"
		labelsJoin = ""
	}

	return fmt.Sprintf(`
		WITH base AS (
			SELECT i.* FROM %s i
			%s
			%s
			%s
		)
		SELECT %s,
			%s,
			COALESCE(dc.cnt, 0) AS dep_count,
			COALESCE(rc.cnt, 0) AS rdep_count,
			COALESCE(cc.cnt, 0) AS comment_count,
			pc.parent_id     AS parent_id,
			d.deps_json      AS deps_json
		FROM base i
		%s
		LEFT JOIN (
			SELECT issue_id, COUNT(*) AS cnt
			FROM %s
			WHERE type = 'blocks' AND issue_id IN (SELECT id FROM base)
			GROUP BY issue_id
		) dc ON dc.issue_id = i.id
		LEFT JOIN (
			SELECT dep_id, COUNT(*) AS cnt FROM (
				%s
			) all_blockers WHERE dep_id IN (SELECT id FROM base) GROUP BY dep_id
		) rc ON rc.dep_id = i.id
		LEFT JOIN (
			SELECT issue_id, COUNT(*) AS cnt
			FROM %s
			WHERE issue_id IN (SELECT id FROM base)
			GROUP BY issue_id
		) cc ON cc.issue_id = i.id
		LEFT JOIN (
			SELECT issue_id,
			       MIN(COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external)) AS parent_id
			FROM %s
			WHERE type = 'parent-child' AND issue_id IN (SELECT id FROM base)
			GROUP BY issue_id
		) pc ON pc.issue_id = i.id
		LEFT JOIN (
			SELECT issue_id, JSON_ARRAYAGG(%s) AS deps_json
			FROM %s
			WHERE issue_id IN (SELECT id FROM base)
			GROUP BY issue_id
		) d ON d.issue_id = i.id
		%s
	`,
		tables.Main,
		whereSQL,
		orderBySQL,
		limitSQL,
		ReadyWorkIssueColumns,
		labelsSelect,
		labelsJoin,
		tables.Dependencies,
		reverseBlockerSelect,
		tables.Comments,
		tables.Dependencies,
		DepJSONObject,
		tables.Dependencies,
		orderBySQL,
	)
}
