package validation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// auditedColumns is the set of cells whose change between two runs causes
// the row to be marked StateChanged. The other columns (Notes, Change Log,
// Schema Source) are derived/metadata and never trigger reconciliation.
var auditedColumns = map[string]func(AuditRow) string{
	"Schema-Backed":           func(r AuditRow) string { return r.SchemaBacked },
	"Schema-Driven (Meshery)": func(r AuditRow) string { return r.SchemaDrivenMeshery },
	"Schema-Driven (Cloud)":   func(r AuditRow) string { return r.SchemaDrivenCloud },
}

// reconcileKey for reconciliation: (Endpoint, Method) per architecture §10.2.
type reconcileKey struct {
	Endpoint string
	Method   string
}

func keyOf(r AuditRow) reconcileKey {
	return reconcileKey{Endpoint: r.Endpoint, Method: r.Method}
}

// reconcile compares the current audit rows against a previous serialized
// view (sheet rows or local CSV cache) and produces tracked endpoints with
// state transitions. It is pure logic — no I/O — so it is fully testable.
func reconcile(current []AuditRow, previous [][]string) []TrackedEndpoint {
	today := time.Now().Format("2006-01-02")

	prevRows := parsePreviousRows(previous)
	prevByKey := make(map[reconcileKey]AuditRow, len(prevRows))
	for _, r := range prevRows {
		prevByKey[keyOf(r)] = r
	}

	tracked := make([]TrackedEndpoint, 0, len(current)+len(prevRows))
	seen := make(map[reconcileKey]bool, len(current))

	for _, cur := range current {
		key := keyOf(cur)
		seen[key] = true
		prev, exists := prevByKey[key]
		if !exists {
			tracked = append(tracked, TrackedEndpoint{
				Row:       withChangeLog(cur, fmt.Sprintf("+added %s", today)),
				State:     StateNew,
				ChangeLog: fmt.Sprintf("+added %s", today),
			})
			continue
		}
		changed := changedColumns(prev, cur)
		if len(changed) == 0 {
			tracked = append(tracked, TrackedEndpoint{
				Row:       withChangeLog(cur, prev.ChangeLog),
				State:     StateExisting,
				ChangeLog: prev.ChangeLog,
			})
			continue
		}
		log := fmt.Sprintf("~changed %s: %s", today, strings.Join(changed, ", "))
		tracked = append(tracked, TrackedEndpoint{
			Row:       withChangeLog(cur, log),
			State:     StateChanged,
			ChangeLog: log,
		})
	}

	// Carry over rows that are in previous but absent from current.
	for _, r := range prevRows {
		if seen[keyOf(r)] {
			continue
		}
		log := fmt.Sprintf("-removed %s", today)
		tracked = append(tracked, TrackedEndpoint{
			Row:       withChangeLog(r, log),
			State:     StateDeleted,
			ChangeLog: log,
		})
	}

	return tracked
}

// parsePreviousRows accepts the raw [][]string we received from a sheet read
// or CSV file. It strips a header row if present (first column == "Category"
// is the canonical header) and converts each row into an AuditRow.
func parsePreviousRows(rows [][]string) []AuditRow {
	if len(rows) == 0 {
		return nil
	}
	start := 0
	if len(rows[0]) > 0 && rows[0][0] == "Category" {
		start = 1
	}
	out := make([]AuditRow, 0, len(rows)-start)
	for _, r := range rows[start:] {
		if len(r) == 0 {
			continue
		}
		out = append(out, rowFromStrings(r))
	}
	return out
}

// changedColumns compares the audited columns of two rows and returns the
// names of any that differ.
func changedColumns(a, b AuditRow) []string {
	var changed []string
	for name, f := range auditedColumns {
		if f(a) != f(b) {
			changed = append(changed, name)
		}
	}
	return changed
}

func withChangeLog(r AuditRow, log string) AuditRow {
	r.ChangeLog = log
	return r
}

// trackedToCSV converts a slice of TrackedEndpoints back into the [][]string
// shape that downstream sheet/CSV writers expect (header + rows).
func trackedToCSV(tracked []TrackedEndpoint) [][]string {
	rows := make([][]string, 0, len(tracked)+1)
	rows = append(rows, append([]string(nil), auditCSVHeader...))
	for _, t := range tracked {
		rows = append(rows, t.Row.toRow())
	}
	return rows
}

// rowsToCSV converts plain audit rows (no reconciliation) into the
// header+rows shape used by CSV/sheet writers.
func rowsToCSV(rows []AuditRow) [][]string {
	out := make([][]string, 0, len(rows)+1)
	out = append(out, append([]string(nil), auditCSVHeader...))
	for _, r := range rows {
		out = append(out, r.toRow())
	}
	return out
}

// assertCanonicalInputs guards canonical sheet writes against accidental
// non-canonical inputs. The audit refuses to write to the canonical sheet
// when the user has overridden one of the consumer repo paths, because the
// resulting state would diverge from what an upstream run would produce.
//
// Fork contributors can still write to a personal --sheet-id; this guard
// only fires when the canonical sheet is targeted.
func assertCanonicalInputs(opts APIAuditOptions) error {
	if opts.SheetID == "" {
		return nil
	}
	var nonCanonical []string
	if opts.MesheryRepo != "" {
		nonCanonical = append(nonCanonical, "--meshery-repo")
	}
	if opts.CloudRepo != "" {
		nonCanonical = append(nonCanonical, "--cloud-repo")
	}
	if len(nonCanonical) > 0 {
		return fmt.Errorf(
			"api-audit: refusing canonical sheet write with non-canonical inputs (%s); "+
				"omit them, or use a personal --sheet-id",
			strings.Join(nonCanonical, ", "))
	}
	return nil
}

// readSheet pulls every value out of the first sheet (range "A1:Z10000") of
// the given spreadsheet. The returned rows are exactly what reconcile expects.
func readSheet(ctx context.Context, sheetID string, creds []byte) ([][]string, error) {
	srv, err := newSheetsService(ctx, creds)
	if err != nil {
		return nil, err
	}
	resp, err := srv.Spreadsheets.Values.Get(sheetID, "A1:Z10000").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("read sheet: %w", err)
	}
	rows := make([][]string, 0, len(resp.Values))
	for _, raw := range resp.Values {
		row := make([]string, 0, len(raw))
		for _, cell := range raw {
			row = append(row, fmt.Sprint(cell))
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// writeSheet clears the destination sheet and writes the reconciled rows to
// it. Deleted rows are preserved with a "-removed" change log so the sheet
// retains historical state.
func writeSheet(ctx context.Context, sheetID string, creds []byte, tracked []TrackedEndpoint) error {
	srv, err := newSheetsService(ctx, creds)
	if err != nil {
		return err
	}
	_, err = srv.Spreadsheets.Values.Clear(sheetID, "A1:Z10000", &sheets.ClearValuesRequest{}).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("clear sheet: %w", err)
	}

	rows := trackedToCSV(tracked)
	values := make([][]any, 0, len(rows))
	for _, r := range rows {
		row := make([]any, 0, len(r))
		for _, cell := range r {
			row = append(row, cell)
		}
		values = append(values, row)
	}

	_, err = srv.Spreadsheets.Values.Update(sheetID, "A1", &sheets.ValueRange{
		Values: values,
	}).ValueInputOption("RAW").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("update sheet: %w", err)
	}
	return nil
}

// newSheetsService builds a Google Sheets client from a JSON credentials blob.
// It expects either service-account credentials or any other format
// google.CredentialsFromJSON understands.
func newSheetsService(ctx context.Context, creds []byte) (*sheets.Service, error) {
	if len(creds) == 0 {
		return nil, fmt.Errorf("api-audit: empty Google credentials")
	}
	gc, err := google.CredentialsFromJSON(ctx, creds, sheets.SpreadsheetsScope)
	if err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	srv, err := sheets.NewService(ctx, option.WithCredentials(gc))
	if err != nil {
		return nil, fmt.Errorf("sheets client: %w", err)
	}
	return srv, nil
}

// init wires sheets-aware reconciliation into the orchestrator. apiaudit.go
// declares applyReconciliation as a stub variable; sheets.go replaces it at
// package init time so apiaudit.go has no compile-time dependency on the
// google sheets packages.
func init() {
	applyReconciliation = reconcileFromOpts
}

// reconcileFromOpts is the runtime hook installed into apiaudit.go's
// applyReconciliation. It implements step 6 of the data flow:
//
//   - SheetID set    → guard, read sheet, reconcile, write sheet, install Tracked
//   - PreviousRows   → reconcile in-memory only (dry-run with local CSV cache)
//   - neither        → no-op
func reconcileFromOpts(opts APIAuditOptions, result *APIAuditResult) error {
	if result == nil {
		return nil
	}

	if opts.SheetID != "" {
		if err := assertCanonicalInputs(opts); err != nil {
			return err
		}
		ctx := context.Background()
		previous, err := readSheet(ctx, opts.SheetID, opts.SheetsCredentials)
		if err != nil {
			return err
		}
		tracked := reconcile(result.Rows, previous)
		if err := writeSheet(ctx, opts.SheetID, opts.SheetsCredentials, tracked); err != nil {
			return err
		}
		result.Tracked = tracked
		return nil
	}

	if len(opts.PreviousRows) > 0 {
		result.Tracked = reconcile(result.Rows, opts.PreviousRows)
		return nil
	}

	return nil
}
