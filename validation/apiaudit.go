// Package validation hosts both the schema design validator and the API
// audit pipeline. The audit compares schemas in this repo against handler
// implementations in meshery/meshery and meshery-cloud and reports coverage
// per endpoint.
//
// The exported surface area of the audit is intentionally tiny:
//
//	validation.RunAPIAudit(opts) (*APIAuditResult, error)
//	validation.APIAuditOptions
//	validation.APIAuditResult
//
// Everything else is unexported. The CLI in cmd/api-audit consumes only
// these symbols.
package validation

import (
	"fmt"
	"sort"
	"strings"
)

// APIAuditOptions configures a single audit run.
type APIAuditOptions struct {
	// Schema repo root (required).
	RootDir string

	// Consumer repos. Empty = skip that consumer.
	MesheryRepo string
	CloudRepo   string

	// Google Sheets update. Empty = no sheet interaction (dry run).
	SheetID           string
	SheetsCredentials []byte

	// Previous state for dry-run reconciliation. Passed by cmd/api-audit
	// from the local CSV cache. Nil = no diff available.
	PreviousRows [][]string

	Verbose bool
}

// APIAuditResult is the output of RunAPIAudit.
type APIAuditResult struct {
	// Analysis results.
	SchemaIndex *schemaIndex
	Match       *matchResult

	// Reconciled state (nil if no previous state was provided).
	Tracked []TrackedEndpoint

	// Output rows for CSV/sheet (sorted, deterministic).
	Rows []AuditRow

	// Summary counts for terminal display.
	Summary auditSummary
}

// AuditRow is one row of the audit output, matching the sheet column schema
// exactly. See section 11.1 of the architecture doc.
type AuditRow struct {
	Category            string
	SubCategory         string
	Endpoint            string
	Method              string
	SchemaBacked        string
	SchemaDrivenMeshery string
	SchemaDrivenCloud   string
	Notes               string
	ChangeLog           string
	SchemaSource        string
}

// auditCSVHeader is the canonical header for CSV/sheet output.
var auditCSVHeader = []string{
	"Category",
	"Sub-Category",
	"Endpoint",
	"Method",
	"Schema-Backed",
	"Schema-Driven (Meshery)",
	"Schema-Driven (Cloud)",
	"Notes",
	"Change Log",
	"Schema Source",
}

// toRow converts the audit row to its serialized string slice.
func (r AuditRow) toRow() []string {
	return []string{
		r.Category,
		r.SubCategory,
		r.Endpoint,
		r.Method,
		r.SchemaBacked,
		r.SchemaDrivenMeshery,
		r.SchemaDrivenCloud,
		r.Notes,
		r.ChangeLog,
		r.SchemaSource,
	}
}

// rowFromStrings reconstructs an AuditRow from a serialized string slice.
// Missing trailing columns are tolerated.
func rowFromStrings(cols []string) AuditRow {
	get := func(i int) string {
		if i < len(cols) {
			return cols[i]
		}
		return ""
	}
	return AuditRow{
		Category:            get(0),
		SubCategory:         get(1),
		Endpoint:            get(2),
		Method:              get(3),
		SchemaBacked:        get(4),
		SchemaDrivenMeshery: get(5),
		SchemaDrivenCloud:   get(6),
		Notes:               get(7),
		ChangeLog:           get(8),
		SchemaSource:        get(9),
	}
}

// EndpointState enumerates the four reconciliation states an audit row can
// be in. The full reconciliation logic lives in sheets.go (Session 2);
// declaring the type here keeps APIAuditResult self-contained.
type EndpointState int

const (
	StateNew EndpointState = iota
	StateExisting
	StateChanged
	StateDeleted
)

// TrackedEndpoint is one reconciled row with state transition. The CLI
// consumes this to render the diff section; fields are intentionally simple.
type TrackedEndpoint struct {
	Row       AuditRow
	State     EndpointState
	ChangeLog string
}

// auditSummary captures the high-level counts shown in the terminal table.
type auditSummary struct {
	SchemaEndpoints      int
	MesheryEndpoints     int
	CloudEndpoints       int
	Matched              int
	SchemaOnly           int
	ConsumerOnly         int
	SchemaBackedTrue     int
	SchemaDrivenTrue     int
	SchemaDrivenPartial  int
	SchemaDrivenFalse    int
	SchemaDrivenNotAud   int
	MesheryBackedTrue    int
	MesheryDrivenTrue    int
	MesheryDrivenPartial int
	MesheryDrivenFalse   int
	MesheryDrivenNotAud  int
	CloudBackedTrue      int
	CloudDrivenTrue      int
	CloudDrivenPartial   int
	CloudDrivenFalse     int
	CloudDrivenNotAud    int
}

// CSVRows returns the audit output as a header-plus-rows [][]string suitable
// for csv.Writer.WriteAll. When reconciliation has run, the reconciled rows
// are used so the emitted Change Log column reflects state transitions;
// otherwise the plain analysis rows are returned.
func (r *APIAuditResult) CSVRows() [][]string {
	if r == nil {
		return [][]string{append([]string(nil), auditCSVHeader...)}
	}
	if len(r.Tracked) > 0 {
		return trackedToCSV(r.Tracked)
	}
	return rowsToCSV(r.Rows)
}

// RunAPIAudit is the single entry point for the API audit pipeline.
func RunAPIAudit(opts APIAuditOptions) (*APIAuditResult, error) {
	return runAPIAudit(opts, nil, nil)
}

// runAPIAudit is the test-friendly version that accepts pre-built sourceTrees
// in place of repo paths. RunAPIAudit wraps this with localTree instances.
func runAPIAudit(opts APIAuditOptions, mesheryTree, cloudTree sourceTree) (*APIAuditResult, error) {
	if opts.RootDir == "" {
		return nil, fmt.Errorf("api-audit: RootDir is required")
	}

	idx, err := buildEndpointIndex(opts.RootDir)
	if err != nil {
		return nil, fmt.Errorf("api-audit: build endpoint index: %w", err)
	}

	if mesheryTree == nil && opts.MesheryRepo != "" {
		mesheryTree = localTree{root: opts.MesheryRepo}
	}
	if cloudTree == nil && opts.CloudRepo != "" {
		cloudTree = localTree{root: opts.CloudRepo}
	}

	// Build the meshery-schemas Go-type index once. This drives field-level
	// verification of payloads in handlers that decode into a schemas type;
	// without it verifyShape always falls through to shapeUnverified.
	schemaTypes := loadSchemasGoTypes(opts.RootDir)

	var mesheryEndpoints []consumerEndpoint
	if mesheryTree != nil {
		mesheryEndpoints, err = parseGorillaRoutes(mesheryTree)
		if err != nil {
			return nil, fmt.Errorf("api-audit: parse meshery routes: %w", err)
		}
		mesheryEndpoints = indexHandlers(mesheryTree, mesheryEndpoints, schemaTypes)
	}

	var cloudEndpoints []consumerEndpoint
	if cloudTree != nil {
		cloudEndpoints, err = parseEchoRoutes(cloudTree)
		if err != nil {
			return nil, fmt.Errorf("api-audit: parse cloud routes: %w", err)
		}
		cloudEndpoints = indexHandlers(cloudTree, cloudEndpoints, schemaTypes)
	}

	match := matchEndpoints(idx, mesheryEndpoints, cloudEndpoints)

	mesheryProvided := mesheryTree != nil
	cloudProvided := cloudTree != nil

	rows := buildAuditRows(idx, match, mesheryEndpoints, cloudEndpoints, mesheryProvided, cloudProvided)
	sortAuditRows(rows)

	summary := computeSummary(idx, mesheryEndpoints, cloudEndpoints, match, rows, mesheryProvided, cloudProvided)

	result := &APIAuditResult{
		SchemaIndex: idx,
		Match:       match,
		Rows:        rows,
		Summary:     summary,
	}

	// Step 6 — sheet/reconciliation. Implemented in sheets.go in Session 2.
	if err := applyReconciliation(opts, result); err != nil {
		return result, err
	}

	return result, nil
}

// buildAuditRows materializes one AuditRow per endpoint, joining schema and
// consumer info. The result is unsorted; sortAuditRows is the canonical
// ordering used everywhere downstream.
func buildAuditRows(
	idx *schemaIndex,
	match *matchResult,
	mesheryEndpoints, cloudEndpoints []consumerEndpoint,
	mesheryProvided, cloudProvided bool,
) []AuditRow {
	rows := make([]AuditRow, 0, len(idx.Endpoints)+len(match.ConsumerOnly))

	// Schema-defined endpoints (matched + schema-only).
	for _, ep := range idx.Endpoints {
		var matchedConsumers []consumerEndpoint
		for _, m := range match.Matched {
			if m.Schema.SourceFile == ep.SourceFile && m.Schema.Method == ep.Method && m.Schema.Path == ep.Path {
				matchedConsumers = m.Consumers
				break
			}
		}

		row := newSchemaRow(ep, matchedConsumers, mesheryProvided, cloudProvided)
		rows = append(rows, row)
	}

	// Consumer-only rows (no schema endpoint).
	for _, c := range match.ConsumerOnly {
		row := newConsumerOnlyRow(c, mesheryProvided, cloudProvided)
		rows = append(rows, row)
	}
	return rows
}

func newSchemaRow(ep schemaEndpoint, consumers []consumerEndpoint, mesheryProvided, cloudProvided bool) AuditRow {
	row := AuditRow{
		Category:     categoryFromTags(ep.Tags),
		SubCategory:  ep.Construct,
		Endpoint:     ep.Path,
		Method:       ep.Method,
		SchemaBacked: classifySchemaBacked(true, ep),
		SchemaSource: ep.SourceFile,
	}

	mesheryConsumer := pickConsumer(consumers, "meshery")
	cloudConsumer := pickConsumer(consumers, "meshery-cloud")

	mesheryAllowed := xInternalAllows(ep.XInternal, "meshery")
	cloudAllowed := xInternalAllows(ep.XInternal, "cloud")

	if !mesheryProvided || !mesheryAllowed {
		row.SchemaDrivenMeshery = "N/A"
	} else {
		row.SchemaDrivenMeshery = classifySchemaDriven(true, mesheryConsumer, ep.RequestShape, ep.ResponseShape)
	}

	if !cloudProvided || !cloudAllowed {
		row.SchemaDrivenCloud = "N/A"
	} else {
		row.SchemaDrivenCloud = classifySchemaDriven(true, cloudConsumer, ep.RequestShape, ep.ResponseShape)
	}

	row.Notes = buildNotes(ep, mesheryConsumer, cloudConsumer, mesheryAllowed, cloudAllowed)
	return row
}

func newConsumerOnlyRow(c consumerEndpoint, mesheryProvided, cloudProvided bool) AuditRow {
	row := AuditRow{
		Category:     "Uncategorized",
		SubCategory:  "(consumer-only)",
		Endpoint:     c.Path,
		Method:       c.Method,
		SchemaBacked: "FALSE",
	}
	notes := []string{"schema not defined"}
	if c.HandlerName == "" || c.HandlerName == "(anonymous)" {
		notes = append(notes, "anonymous handler; cannot determine schema usage")
	}
	if len(c.Notes) > 0 {
		notes = append(notes, c.Notes...)
	}
	row.Notes = strings.Join(notes, "; ")

	switch c.Repo {
	case "meshery":
		row.SchemaDrivenMeshery = classifySchemaDriven(true, &c, nil, nil)
		if cloudProvided {
			row.SchemaDrivenCloud = "Not Audited"
		} else {
			row.SchemaDrivenCloud = "N/A"
		}
	case "meshery-cloud":
		row.SchemaDrivenCloud = classifySchemaDriven(true, &c, nil, nil)
		if mesheryProvided {
			row.SchemaDrivenMeshery = "Not Audited"
		} else {
			row.SchemaDrivenMeshery = "N/A"
		}
	default:
		row.SchemaDrivenMeshery = "N/A"
		row.SchemaDrivenCloud = "N/A"
	}
	return row
}

// pickConsumer returns the first consumer entry from a matched bundle that
// belongs to the named repo, or nil if absent.
func pickConsumer(consumers []consumerEndpoint, repo string) *consumerEndpoint {
	for i := range consumers {
		if consumers[i].Repo == repo {
			return &consumers[i]
		}
	}
	return nil
}

// categoryFromTags maps an operation's first tag (or "Uncategorized") to the
// Category column. The schema is the source of truth — no fallback table.
func categoryFromTags(tags []string) string {
	if len(tags) == 0 {
		return "Uncategorized"
	}
	return tags[0]
}

func buildNotes(ep schemaEndpoint, meshery, cloud *consumerEndpoint, mesheryAllowed, cloudAllowed bool) string {
	var notes []string
	if ep.Deprecated {
		notes = append(notes, "deprecated")
	}
	if ep.Public {
		notes = append(notes, "explicitly public")
	}
	if !mesheryAllowed {
		notes = append(notes, "cloud-only endpoint")
	}
	if !cloudAllowed {
		notes = append(notes, "meshery-only endpoint")
	}
	if mesheryAllowed && meshery == nil {
		notes = append(notes, "handler not found in meshery")
	}
	if cloudAllowed && cloud == nil {
		notes = append(notes, "handler not found in meshery-cloud")
	}
	for _, c := range []*consumerEndpoint{meshery, cloud} {
		if c == nil {
			continue
		}
		for _, n := range c.Notes {
			if n != "" {
				notes = append(notes, n)
			}
		}
	}
	return strings.Join(uniqueStrings(notes), "; ")
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// sortAuditRows orders rows by (Category, SubCategory, Endpoint, Method).
func sortAuditRows(rows []AuditRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Category != rows[j].Category {
			return rows[i].Category < rows[j].Category
		}
		if rows[i].SubCategory != rows[j].SubCategory {
			return rows[i].SubCategory < rows[j].SubCategory
		}
		if rows[i].Endpoint != rows[j].Endpoint {
			return rows[i].Endpoint < rows[j].Endpoint
		}
		return rows[i].Method < rows[j].Method
	})
}

func computeSummary(
	idx *schemaIndex,
	meshery, cloud []consumerEndpoint,
	match *matchResult,
	rows []AuditRow,
	mesheryProvided, cloudProvided bool,
) auditSummary {
	s := auditSummary{
		SchemaEndpoints:  len(idx.Endpoints),
		MesheryEndpoints: len(meshery),
		CloudEndpoints:   len(cloud),
		Matched:          len(match.Matched),
		SchemaOnly:       len(match.SchemaOnly),
		ConsumerOnly:     len(match.ConsumerOnly),
	}
	for _, r := range rows {
		if r.SchemaBacked == "TRUE" {
			s.SchemaBackedTrue++
		}
		if mesheryProvided {
			// Per-repo Schema-Backed = TRUE count: a row contributes
			// when the schema endpoint has a 2xx $ref (SchemaBacked
			// == "TRUE") AND Meshery is in scope for that row (i.e.
			// Schema-Driven (Meshery) is not "N/A").
			if r.SchemaBacked == "TRUE" && r.SchemaDrivenMeshery != "N/A" {
				s.MesheryBackedTrue++
			}
			switch r.SchemaDrivenMeshery {
			case "TRUE":
				s.MesheryDrivenTrue++
				s.SchemaDrivenTrue++
			case "Partial":
				s.MesheryDrivenPartial++
				s.SchemaDrivenPartial++
			case "FALSE":
				s.MesheryDrivenFalse++
				s.SchemaDrivenFalse++
			case "Not Audited":
				s.MesheryDrivenNotAud++
				s.SchemaDrivenNotAud++
			}
		}
		if cloudProvided {
			if r.SchemaBacked == "TRUE" && r.SchemaDrivenCloud != "N/A" {
				s.CloudBackedTrue++
			}
			switch r.SchemaDrivenCloud {
			case "TRUE":
				s.CloudDrivenTrue++
				s.SchemaDrivenTrue++
			case "Partial":
				s.CloudDrivenPartial++
				s.SchemaDrivenPartial++
			case "FALSE":
				s.CloudDrivenFalse++
				s.SchemaDrivenFalse++
			case "Not Audited":
				s.CloudDrivenNotAud++
				s.SchemaDrivenNotAud++
			}
		}
	}
	return s
}

// applyReconciliation is a stub that gets a real implementation in sheets.go
// during Session 2. Defining it here keeps the orchestrator self-contained
// and avoids forcing the test build to import sheets-only symbols.
var applyReconciliation = func(opts APIAuditOptions, result *APIAuditResult) error {
	return nil
}
