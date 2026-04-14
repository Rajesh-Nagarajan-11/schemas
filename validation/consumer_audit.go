package validation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Sheet layout constants. Cols 0..12 are the primary human-facing view
// (A..M); cols 13..24 (N..Y) are intentionally left blank so the sheet
// looks uncluttered; col 25 (Z) holds machine-only metadata as JSON.
const (
	metadataColumnIndex = 25
	totalColumns        = 26
)

// RowMetadata is the opaque JSON blob stored in column Z of each data
// row. It is machine-only — no human reads column Z directly.
type RowMetadata struct {
	State          string   `json:"state,omitempty"`
	ChangedColumns []string `json:"changed_columns,omitempty"`
	FirstSeen      string   `json:"first_seen,omitempty"`
	LastReconciled string   `json:"last_reconciled,omitempty"`
}

// isZero reports whether no metadata fields are set.
func (m RowMetadata) isZero() bool {
	return m.State == "" && len(m.ChangedColumns) == 0 && m.FirstSeen == "" && m.LastReconciled == ""
}

// encode serializes the metadata to compact JSON. Empty metadata
// encodes to an empty string rather than "{}" so the sheet cell stays
// clean for rows that have no recorded history.
func (m RowMetadata) encode() string {
	if m.isZero() {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeRowMetadata parses a column-Z cell. An empty or malformed blob
// yields a zero RowMetadata — the reconciler will repopulate it.
func decodeRowMetadata(s string) RowMetadata {
	s = strings.TrimSpace(s)
	if s == "" {
		return RowMetadata{}
	}
	var m RowMetadata
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return RowMetadata{}
	}
	return m
}

// DeletionRecord is one entry in the deletion ledger kept in row 1 of
// column Z. The ledger is append-only and machine-only.
type DeletionRecord struct {
	Endpoint      string `json:"endpoint"`
	Method        string `json:"method"`
	RemovedAt     string `json:"removed_at"`
	LastChangeLog string `json:"last_change_log,omitempty"`
}

// encodeDeletionLedger serializes the ledger for storage in Z1. An
// empty ledger encodes to an empty string.
func encodeDeletionLedger(ledger []DeletionRecord) string {
	if len(ledger) == 0 {
		return ""
	}
	b, err := json.Marshal(ledger)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeDeletionLedger parses Z1. Unparseable content yields an empty
// ledger; the reconciler will rebuild it.
func decodeDeletionLedger(s string) []DeletionRecord {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []DeletionRecord
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// ConsumerAuditOptions configures a single consumer-audit run.
type ConsumerAuditOptions struct {
	// Schema repo root (required).
	RootDir string

	// Consumer repos. Empty = skip that consumer.
	MesheryRepo string
	CloudRepo   string

	// Google Sheets update. Empty = no sheet interaction (dry run).
	SheetID           string
	SheetsCredentials []byte
}

// ConsumerAuditResult is the output of RunConsumerAudit.
type ConsumerAuditResult struct {
	// Analysis results.
	SchemaIndex *schemaIndex
	Match       *matchResult

	// Reconciled state (nil if no previous state was provided).
	Tracked []TrackedEndpoint

	// NewDeletions are endpoints detected as removed on this run. They
	// have been appended to DeletionLedger already; this slice exists so
	// the CLI can surface them separately in the diff.
	NewDeletions []DeletionRecord

	// DeletionLedger is the full updated ledger that will be written to
	// Z1 of the sheet. Includes NewDeletions plus everything previously
	// recorded.
	DeletionLedger []DeletionRecord

	// Output rows for sheet serialization (sorted, deterministic).
	Rows []AuditRow

	// Summary counts for terminal display.
	Summary auditSummary
}

// ConsumerAuditRow is one row of the audit output.
type ConsumerAuditRow struct {
	Category            string
	SubCategory         string
	Endpoint            string
	Method              string
	EndpointStatus      string
	XAnnotated          string
	SchemaBackedMeshery string
	SchemaBackedCloud   string
	SchemaDrivenMeshery string
	SchemaDrivenCloud   string
	Notes               string
	// ChangeLog is the UTC timestamp of the last state transition
	// (new / changed) for this row, in format "YYYY-MM-DD HH:MM:SS UTC".
	// Empty on rows that have never been touched by reconciliation.
	ChangeLog string
	// Metadata is machine-only data serialized as JSON in column Z.
	Metadata RowMetadata
}

// AuditRow remains a short alias used throughout the validation package.
type AuditRow = ConsumerAuditRow

// auditHeader is the canonical header for sheet row output. Columns
// N..Y are blank spacers; column Z holds the deletion ledger in row 1
// and per-row metadata in all other rows.
var auditHeader = func() []string {
	h := make([]string, totalColumns)
	h[0] = "Category"
	h[1] = "Sub-Category"
	h[2] = "Endpoint"
	h[3] = "Method"
	h[4] = "Endpoint Status"
	h[5] = "x-annotated"
	h[6] = "Schema-Backed (Meshery)"
	h[7] = "Schema-Backed (Cloud)"
	h[8] = "Schema-Driven (Meshery)"
	h[9] = "Schema-Driven (Cloud)"
	h[10] = "Notes"
	h[11] = "Change Log"
	h[metadataColumnIndex] = "__metadata__"
	return h
}()

// toRow converts the audit row to its serialized string slice. The
// returned slice is always totalColumns wide, with N..Y padded empty
// and Z carrying the JSON-encoded metadata.
func (r ConsumerAuditRow) toRow() []string {
	cells := make([]string, totalColumns)
	cells[0] = r.Category
	cells[1] = r.SubCategory
	cells[2] = r.Endpoint
	cells[3] = r.Method
	cells[4] = r.EndpointStatus
	cells[5] = r.XAnnotated
	cells[6] = r.SchemaBackedMeshery
	cells[7] = r.SchemaBackedCloud
	cells[8] = r.SchemaDrivenMeshery
	cells[9] = r.SchemaDrivenCloud
	cells[10] = r.Notes
	cells[11] = r.ChangeLog
	cells[metadataColumnIndex] = r.Metadata.encode()
	return cells
}

// rowFromStrings reconstructs an AuditRow from a serialized string slice.
// Missing trailing columns are tolerated. Legacy Change Log prefixes
// ("+added YYYY-MM-DD", "~changed ...", "-removed ...") are recognized
// and migrated on-the-fly: the date is promoted to an RFC3339 timestamp
// and any missing metadata fields are seeded.
func rowFromStrings(cols []string) ConsumerAuditRow {
	get := func(i int) string {
		if i < len(cols) {
			return cols[i]
		}
		return ""
	}
	row := ConsumerAuditRow{
		Category:            get(0),
		SubCategory:         get(1),
		Endpoint:            get(2),
		Method:              get(3),
		EndpointStatus:      get(4),
		XAnnotated:          get(5),
		SchemaBackedMeshery: get(6),
		SchemaBackedCloud:   get(7),
		SchemaDrivenMeshery: get(8),
		SchemaDrivenCloud:   get(9),
		Notes:               get(10),
		ChangeLog:           get(11),
		Metadata:            decodeRowMetadata(get(metadataColumnIndex)),
	}
	row.ChangeLog, row.Metadata = normalizeLegacyChangeLog(row.ChangeLog, row.Metadata)
	return row
}

// isLegacyTombstone reports whether a row loaded from the sheet is a
// "-removed ..." tombstone from the previous schema. Callers use this
// to migrate the tombstone into the deletion ledger and drop the row.
func isLegacyTombstone(changeLog string) bool {
	return strings.HasPrefix(strings.TrimSpace(changeLog), "-removed")
}

// normalizeLegacyChangeLog rewrites the legacy "+added YYYY-MM-DD",
// "~changed YYYY-MM-DD: Col1, Col2" formats into the new readable UTC
// timestamp and seeds any missing metadata fields derived from the
// prefix. Rows already in the new format are returned unchanged.
func normalizeLegacyChangeLog(changeLog string, meta RowMetadata) (string, RowMetadata) {
	trimmed := strings.TrimSpace(changeLog)
	if trimmed == "" {
		return changeLog, meta
	}
	var state, rest string
	switch {
	case strings.HasPrefix(trimmed, "+added "):
		state = "new"
		rest = strings.TrimPrefix(trimmed, "+added ")
	case strings.HasPrefix(trimmed, "~changed "):
		state = "changed"
		rest = strings.TrimPrefix(trimmed, "~changed ")
	case strings.HasPrefix(trimmed, "-removed "):
		state = "removed"
		rest = strings.TrimPrefix(trimmed, "-removed ")
	default:
		return changeLog, meta
	}
	date := rest
	var changedCols []string
	if idx := strings.Index(rest, ":"); idx >= 0 {
		date = strings.TrimSpace(rest[:idx])
		for _, c := range strings.Split(rest[idx+1:], ",") {
			if c = strings.TrimSpace(c); c != "" {
				changedCols = append(changedCols, c)
			}
		}
	} else {
		date = strings.TrimSpace(date)
	}
	ts := date + " 00:00:00 UTC"
	if meta.State == "" {
		meta.State = state
	}
	if meta.FirstSeen == "" {
		meta.FirstSeen = ts
	}
	if len(meta.ChangedColumns) == 0 && len(changedCols) > 0 {
		meta.ChangedColumns = changedCols
	}
	return ts, meta
}

// EndpointState enumerates the reconciliation states a live audit row
// can be in. Deletions are tracked separately in the deletion ledger
// and never appear as a TrackedEndpoint.
type EndpointState int

const (
	StateNew EndpointState = iota
	StateExisting
	StateChanged
)

// TrackedEndpoint is one reconciled row with state transition. The CLI
// consumes this to render the diff section. The Row already carries
// the authoritative ChangeLog timestamp and Metadata blob.
type TrackedEndpoint struct {
	Row   ConsumerAuditRow
	State EndpointState
	// Prev is the row as it appeared on the previous run. Populated only
	// when State == StateChanged, so the CLI can render before/after diffs.
	Prev *ConsumerAuditRow
}

// auditSummary captures the high-level counts shown in the terminal table.
type auditSummary struct {
	SchemaEndpoints     int
	MesheryEndpoints    int
	CloudEndpoints      int
	Matched             int
	SchemaOnly          int
	SchemaOnlyMeshery   int
	SchemaOnlyCloud     int
	ConsumerOnly        int
	ConsumerOnlyMeshery int
	ConsumerOnlyCloud   int
	Meshery             repoTally
	Cloud               repoTally
}

// SheetRows returns the audit output as header-plus-rows [][]string suitable
// for Google Sheets writes. When reconciliation has run, the reconciled rows
// are used so the emitted Change Log column reflects state transitions;
// otherwise the plain analysis rows are returned.
func (r *ConsumerAuditResult) SheetRows() [][]string {
	if r == nil {
		return [][]string{append([]string(nil), auditHeader...)}
	}
	if len(r.Tracked) > 0 {
		return trackedToSheetRows(r.Tracked, r.DeletionLedger)
	}
	return rowsToSheetRows(r.Rows, r.DeletionLedger)
}

// RunConsumerAudit is the single entry point for the consumer audit pipeline.
func RunConsumerAudit(opts ConsumerAuditOptions) (*ConsumerAuditResult, error) {
	return runConsumerAudit(opts, nil, nil)
}

// runConsumerAudit is the test-friendly version that accepts pre-built
// sourceTrees in place of repo paths.
func runConsumerAudit(opts ConsumerAuditOptions, mesheryTree, cloudTree sourceTree) (*ConsumerAuditResult, error) {
	if opts.RootDir == "" {
		return nil, fmt.Errorf("consumer-audit: RootDir is required")
	}

	idx, err := buildEndpointIndex(opts.RootDir)
	if err != nil {
		return nil, fmt.Errorf("consumer-audit: build endpoint index: %w", err)
	}

	if mesheryTree == nil && opts.MesheryRepo != "" {
		mesheryTree = localTree{root: opts.MesheryRepo}
	}
	if cloudTree == nil && opts.CloudRepo != "" {
		cloudTree = localTree{root: opts.CloudRepo}
	}

	var mesheryEndpoints []consumerEndpoint
	if mesheryTree != nil {
		mesheryEndpoints, err = parseGorillaRoutes(mesheryTree)
		if err != nil {
			return nil, fmt.Errorf("consumer-audit: parse meshery routes: %w", err)
		}
		mesheryEndpoints = indexHandlers(mesheryTree, mesheryEndpoints)
	}

	var cloudEndpoints []consumerEndpoint
	if cloudTree != nil {
		cloudEndpoints, err = parseEchoRoutes(cloudTree)
		if err != nil {
			return nil, fmt.Errorf("consumer-audit: parse cloud routes: %w", err)
		}
		cloudEndpoints = indexHandlers(cloudTree, cloudEndpoints)
	}

	match := matchEndpoints(idx, mesheryEndpoints, cloudEndpoints)

	mesheryProvided := mesheryTree != nil
	cloudProvided := cloudTree != nil

	rows := buildAuditRows(idx, match, mesheryEndpoints, cloudEndpoints, mesheryProvided, cloudProvided)
	sortAuditRows(rows)

	summary := computeSummary(idx, mesheryEndpoints, cloudEndpoints, match, rows, mesheryProvided, cloudProvided)

	result := &ConsumerAuditResult{
		SchemaIndex: idx,
		Match:       match,
		Rows:        rows,
		Summary:     summary,
	}

	if err := reconcileFromOpts(opts, result); err != nil {
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
) []ConsumerAuditRow {
	rows := make([]AuditRow, 0, len(idx.Endpoints)+len(match.ConsumerOnly))
	matchIndex := make(map[schemaRowKey]endpointMatch, len(match.Matched))
	for _, m := range match.Matched {
		matchIndex[schemaRowKeyOf(m.Schema)] = m
	}

	type auditDisplayKey struct {
		Endpoint string
		Method   string
	}
	consumerOnlyByKey := make(map[auditDisplayKey][]consumerEndpoint, len(match.ConsumerOnly))
	for _, c := range match.ConsumerOnly {
		key := auditDisplayKey{Endpoint: c.Path, Method: c.Method}
		consumerOnlyByKey[key] = append(consumerOnlyByKey[key], c)
	}

	// Schema-defined endpoints (matched + schema-only).
	for _, ep := range idx.Endpoints {
		key := auditDisplayKey{Endpoint: ep.Path, Method: ep.Method}
		consumers := append([]consumerEndpoint(nil), matchIndex[schemaRowKeyOf(ep)].Consumers...)
		if extra := consumerOnlyByKey[key]; len(extra) > 0 {
			consumers = append(consumers, extra...)
			delete(consumerOnlyByKey, key)
		}
		row := newSchemaRow(ep, consumers, mesheryProvided, cloudProvided)
		rows = append(rows, row)
	}

	// Consumer-only rows are consolidated by (method, path) so the same
	// schema-less endpoint implemented in both consumers appears once.
	for _, consumers := range consumerOnlyByKey {
		row := newConsumerOnlyRow(consumers, mesheryProvided, cloudProvided)
		rows = append(rows, row)
	}
	return rows
}

type schemaRowKey struct {
	SourceFile string
	Method     string
	Path       string
}

func schemaRowKeyOf(ep schemaEndpoint) schemaRowKey {
	return schemaRowKey{SourceFile: ep.SourceFile, Method: ep.Method, Path: ep.Path}
}

func newSchemaRow(ep schemaEndpoint, consumers []consumerEndpoint, mesheryProvided, cloudProvided bool) ConsumerAuditRow {
	row := ConsumerAuditRow{
		Category:    categoryFromTags(ep.Tags),
		SubCategory: ep.Construct,
		Endpoint:    ep.Path,
		Method:      ep.Method,
		XAnnotated:  classifyXAnnotated(ep.XInternal),
	}

	mesheryAllowed := xInternalAllows(ep.XInternal, "meshery")
	cloudAllowed := xInternalAllows(ep.XInternal, "cloud")

	schemaNote := classifySchemaNote(ep)

	mesheryConsumers := filterConsumersByRepo(consumers, "meshery")
	cloudConsumers := filterConsumersByRepo(consumers, "meshery-cloud")
	mesheryAssessment := assessConsumers(mesheryProvided && mesheryAllowed, "meshery", mesheryConsumers, ep.RequestShape, ep.ResponseShape)
	cloudAssessment := assessConsumers(cloudProvided && cloudAllowed, "meshery-cloud", cloudConsumers, ep.RequestShape, ep.ResponseShape)

	row.EndpointStatus = computeEndpointStatus(true, mesheryAllowed, cloudAllowed, len(mesheryConsumers) > 0, len(cloudConsumers) > 0)
	row.SchemaBackedMeshery = schemaBackedFor(mesheryProvided, mesheryAllowed, mesheryConsumers)
	row.SchemaBackedCloud = schemaBackedFor(cloudProvided, cloudAllowed, cloudConsumers)

	// Schema-Driven is only meaningful when the handler actually imports
	// the schema (Schema-Backed == TRUE). If the handler doesn't, the
	// "drift" is just that it was never wired to the schema — nothing for
	// an auditor to act on in this column. Also blank the cell when the
	// schema itself has neither a comparable request nor response shape.
	bothShapesMissing := ep.RequestShape == nil && ep.ResponseShape == nil
	if row.SchemaBackedMeshery == "TRUE" && !bothShapesMissing {
		row.SchemaDrivenMeshery = normalizeDrivenStatus(mesheryAssessment.Status)
	}
	if row.SchemaBackedCloud == "TRUE" && !bothShapesMissing {
		row.SchemaDrivenCloud = normalizeDrivenStatus(cloudAssessment.Status)
	}

	row.Notes = buildLabeledNotes(schemaNote, mesheryAssessment, cloudAssessment)
	return row
}

func newConsumerOnlyRow(consumers []consumerEndpoint, mesheryProvided, cloudProvided bool) ConsumerAuditRow {
	if len(consumers) == 0 {
		return ConsumerAuditRow{}
	}

	category, subCategory := deriveConsumerLocation(consumers[0].Path)
	row := ConsumerAuditRow{
		Category:    category,
		SubCategory: subCategory,
		Endpoint:    consumers[0].Path,
		Method:      consumers[0].Method,
		XAnnotated:  "No schema",
	}
	mesheryConsumers := filterConsumersByRepo(consumers, "meshery")
	cloudConsumers := filterConsumersByRepo(consumers, "meshery-cloud")

	row.EndpointStatus = computeEndpointStatus(false, false, false, len(mesheryConsumers) > 0, len(cloudConsumers) > 0)

	// No schema → Schema-Backed is FALSE for the repo that registered it, blank
	// for the other repo. Schema-Driven / Schema-Completeness stay blank (no
	// schema to evaluate against).
	if len(mesheryConsumers) > 0 {
		row.SchemaBackedMeshery = "FALSE"
	}
	if len(cloudConsumers) > 0 {
		row.SchemaBackedCloud = "FALSE"
	}

	mesheryAssessment := assessConsumers(mesheryProvided && len(mesheryConsumers) > 0, "meshery", mesheryConsumers, nil, nil)
	cloudAssessment := assessConsumers(cloudProvided && len(cloudConsumers) > 0, "meshery-cloud", cloudConsumers, nil, nil)
	row.Notes = buildConsumerOnlyAggregateNotes(mesheryAssessment, cloudAssessment)
	return row
}

// classifyXAnnotated returns the x-annotated column value derived from an
// endpoint's x-internal list.
func classifyXAnnotated(xInternal []string) string {
	has := func(s string) bool {
		for _, x := range xInternal {
			if x == s {
				return true
			}
		}
		return false
	}
	switch {
	case len(xInternal) == 0:
		return "None"
	case has("meshery") && has("cloud"):
		return "None"
	case has("cloud"):
		return "Cloud only"
	case has("meshery"):
		return "Meshery"
	}
	return "None"
}

// computeEndpointStatus reports whether the endpoint is live in each consumer
// relative to the schema's scope. See the column legend in the audit sheet.
func computeEndpointStatus(schemaPresent, mApplies, cApplies, mActive, cActive bool) string {
	if !schemaPresent {
		switch {
		case mActive && cActive:
			return "Active - Both"
		case mActive:
			return "Active - Meshery Server"
		case cActive:
			return "Active - Meshery Cloud"
		}
		return ""
	}
	switch {
	case mActive && cActive:
		return "Active - Both"
	case mActive && cApplies:
		return "Active - Meshery Server, Unimplemented Meshery Cloud"
	case mActive:
		return "Active - Meshery Server"
	case cActive && mApplies:
		return "Active - Meshery Cloud, Unimplemented Meshery Server"
	case cActive:
		return "Active - Meshery Cloud"
	case mApplies && cApplies:
		return "Unimplemented Both"
	case mApplies:
		return "Unimplemented Meshery Server"
	case cApplies:
		return "Unimplemented Meshery Cloud"
	}
	return ""
}

// schemaBackedFor returns the per-consumer Schema-Backed column value for a
// schema-backed row. Blank means the column does not apply (the consumer was
// not scanned, or the spec does not target that consumer and no handler was
// found). TRUE means the registered handler imports the shared schema types.
func schemaBackedFor(provided, applies bool, repoConsumers []consumerEndpoint) string {
	if !provided {
		return ""
	}
	if !applies && len(repoConsumers) == 0 {
		return ""
	}
	if len(repoConsumers) == 0 {
		return "FALSE"
	}
	for _, c := range repoConsumers {
		if c.ImportsSchemas {
			return "TRUE"
		}
	}
	return "FALSE"
}

// normalizeDrivenStatus folds internal assessment statuses into the small set
// surfaced to the sheet: TRUE, FALSE, or Not Audited. Partial drift is treated
// as FALSE; the diff details live in Notes.
func normalizeDrivenStatus(s string) string {
	if s == "Partial" {
		return "FALSE"
	}
	return s
}

func filterConsumersByRepo(consumers []consumerEndpoint, repo string) []consumerEndpoint {
	out := make([]consumerEndpoint, 0, len(consumers))
	for i := range consumers {
		if consumers[i].Repo == repo {
			out = append(out, consumers[i])
		}
	}
	return out
}

// categoryFromTags maps an operation's first tag (or "Uncategorized") to the
// Category column. The schema is the source of truth — no fallback table.
func categoryFromTags(tags []string) string {
	if len(tags) == 0 {
		return "Uncategorized"
	}
	return tags[0]
}

func deriveConsumerLocation(endpoint string) (string, string) {
	trimmed := strings.Trim(endpoint, "/")
	if trimmed == "" {
		return "Uncategorized", "(consumer-only)"
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 {
		return "Uncategorized", "(consumer-only)"
	}
	if parts[0] == "api" && len(parts) > 1 {
		category := parts[1]
		subCategory := category
		for _, part := range parts[2:] {
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
				continue
			}
			subCategory = part
			break
		}
		return category, subCategory
	}
	return parts[0], parts[0]
}

// labeledNote is one actionable note paired with the source that produced it
// (schema / meshery / cloud). Rendered as "[source] message" one per line.
type labeledNote struct {
	Source  string
	Message string
}

// buildLabeledNotes produces the Notes column for a schema-backed row. Every
// note is attributed to its source and joined by a newline so the sheet shows
// one actionable line per issue.
func buildLabeledNotes(schemaNote string, meshery, cloud consumerAssessment) string {
	var notes []labeledNote
	for _, n := range splitSchemaNote(schemaNote) {
		notes = append(notes, labeledNote{Source: "schema", Message: n})
	}
	notes = append(notes, collectRepoNotes("meshery", "meshery", meshery)...)
	notes = append(notes, collectRepoNotes("cloud", "meshery-cloud", cloud)...)
	return joinLabeledNotes(notes)
}

func buildConsumerOnlyAggregateNotes(meshery, cloud consumerAssessment) string {
	var notes []labeledNote
	notes = append(notes, collectRepoNotes("meshery", "meshery", meshery)...)
	notes = append(notes, collectRepoNotes("cloud", "meshery-cloud", cloud)...)
	return joinLabeledNotes(notes)
}

func collectRepoNotes(source, repo string, a consumerAssessment) []labeledNote {
	var out []labeledNote
	for _, n := range a.Notes {
		out = append(out, labeledNote{Source: source, Message: stripRepoPrefix(n, repo)})
	}
	for _, n := range a.Drift {
		out = append(out, labeledNote{Source: source, Message: stripRepoPrefix(n, repo)})
	}
	return out
}

// stripRepoPrefix removes a "<repo>: " or "<repo> " prefix from a note so the
// repo name is not duplicated with the label tag.
func stripRepoPrefix(note, repo string) string {
	for _, prefix := range []string{repo + ": ", repo + " "} {
		if strings.HasPrefix(note, prefix) {
			return strings.TrimPrefix(note, prefix)
		}
	}
	return note
}

func splitSchemaNote(joined string) []string {
	if joined == "" {
		return nil
	}
	parts := strings.Split(joined, "; ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinLabeledNotes(notes []labeledNote) string {
	seen := make(map[string]bool, len(notes))
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		if n.Message == "" {
			continue
		}
		line := fmt.Sprintf("[%s] %s", n.Source, n.Message)
		if seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return strings.Join(out, "\n")
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
func sortAuditRows(rows []ConsumerAuditRow) {
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

// repoTally holds the per-repo counts surfaced in the terminal report.
type repoTally struct {
	BackedTrue int
}

// tallyRepo derives a repoTally directly from row cells.
func tallyRepo(rows []ConsumerAuditRow, backed func(ConsumerAuditRow) string) repoTally {
	var t repoTally
	for _, r := range rows {
		if backed(r) == "TRUE" {
			t.BackedTrue++
		}
	}
	return t
}

func computeSummary(
	idx *schemaIndex,
	meshery, cloud []consumerEndpoint,
	match *matchResult,
	rows []ConsumerAuditRow,
	mesheryProvided, cloudProvided bool,
) auditSummary {
	s := auditSummary{
		SchemaEndpoints:     len(idx.Endpoints),
		MesheryEndpoints:    len(meshery),
		CloudEndpoints:      len(cloud),
		Matched:             len(match.Matched),
		SchemaOnly:          len(match.SchemaOnly),
		ConsumerOnly:        len(match.ConsumerOnly),
		ConsumerOnlyMeshery: len(filterConsumersByRepo(match.ConsumerOnly, "meshery")),
		ConsumerOnlyCloud:   len(filterConsumersByRepo(match.ConsumerOnly, "meshery-cloud")),
	}
	for _, ep := range match.SchemaOnly {
		if xInternalAllows(ep.XInternal, "meshery") {
			s.SchemaOnlyMeshery++
		}
		if xInternalAllows(ep.XInternal, "cloud") {
			s.SchemaOnlyCloud++
		}
	}
	if mesheryProvided {
		s.Meshery = tallyRepo(rows,
			func(r ConsumerAuditRow) string { return r.SchemaBackedMeshery })
	}
	if cloudProvided {
		s.Cloud = tallyRepo(rows,
			func(r ConsumerAuditRow) string { return r.SchemaBackedCloud })
	}
	return s
}
