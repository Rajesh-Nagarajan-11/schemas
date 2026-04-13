// Command api-audit walks meshery/schemas, joins it against handler
// implementations in meshery/meshery (Gorilla/mux) and meshery-cloud (Echo),
// and reports per-endpoint coverage.
//
// Usage:
//
//	go run ./cmd/api-audit                                                 # schema-only summary
//	go run ./cmd/api-audit --meshery-repo=../meshery --cloud-repo=../meshery-cloud
//	go run ./cmd/api-audit --sheet-id=<id> --credentials=<path>            # canonical sheet write
//	go run ./cmd/api-audit --dry-run > audit.csv                           # no sheet I/O
//
// Session 2 adds Google Sheets integration, the local CSV cache diff, and
// CSV output on stdout.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/meshery/schemas/validation"
)

// cacheFile is the local on-disk CSV cache used for dry-run diffs. It is
// written after a successful sheet update and read at the start of every
// dry-run so contributors can see what would change before pushing.
const cacheFile = ".api-audit-cache.csv"

func main() {
	mesheryRepo := flag.String("meshery-repo", "", "Path to a meshery/meshery checkout (Gorilla router)")
	cloudRepo := flag.String("cloud-repo", "", "Path to a meshery-cloud checkout (Echo router)")
	verbose := flag.Bool("verbose", false, "Print per-construct breakdown and Schema-only / Consumer-only lists")
	sheetID := flag.String("sheet-id", "", "Google Sheet ID to read/write canonical audit state")
	credentials := flag.String("credentials", "", "Path to Google service-account JSON credentials (for --sheet-id)")
	dryRun := flag.Bool("dry-run", false, "Do not touch Google Sheets; diff against local .api-audit-cache.csv and print CSV to stdout")
	flag.Parse()

	rootDir, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "api-audit: could not find repository root: %v\n", err)
		os.Exit(1)
	}

	if *dryRun && *sheetID != "" {
		fmt.Fprintln(os.Stderr, "api-audit: --dry-run and --sheet-id are mutually exclusive")
		os.Exit(1)
	}

	opts := validation.APIAuditOptions{
		RootDir:     rootDir,
		MesheryRepo: *mesheryRepo,
		CloudRepo:   *cloudRepo,
		Verbose:     *verbose,
	}

	if *sheetID != "" {
		if *credentials == "" {
			fmt.Fprintln(os.Stderr, "api-audit: --credentials is required when --sheet-id is set")
			os.Exit(1)
		}
		creds, err := os.ReadFile(*credentials)
		if err != nil {
			fmt.Fprintf(os.Stderr, "api-audit: read credentials: %v\n", err)
			os.Exit(1)
		}
		opts.SheetID = *sheetID
		opts.SheetsCredentials = creds
	}

	if *dryRun {
		cachePath := filepath.Join(rootDir, cacheFile)
		previous, err := readCSVCache(cachePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "api-audit: read cache %s: %v\n", cachePath, err)
			os.Exit(1)
		}
		opts.PreviousRows = previous
	}

	result, err := validation.RunAPIAudit(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "api-audit: %v\n", err)
		os.Exit(1)
	}

	// Only write the summary table to stderr when we're producing CSV
	// on stdout. Otherwise the summary is the primary output.
	summaryOut := io.Writer(os.Stdout)
	if *dryRun {
		summaryOut = os.Stderr
	}
	printSummary(summaryOut, result, *mesheryRepo != "", *cloudRepo != "")

	if *verbose {
		printVerbose(summaryOut, result)
	}

	if len(result.Tracked) > 0 {
		printDiff(summaryOut, result.Tracked)
	}

	if *sheetID != "" {
		// On a successful sheet write, refresh the local cache so the
		// next dry-run diffs against reality.
		if err := writeCSVCache(filepath.Join(rootDir, cacheFile), result); err != nil {
			fmt.Fprintf(os.Stderr, "api-audit: warning: could not refresh %s: %v\n", cacheFile, err)
		}
	}

	if *dryRun {
		if err := writeCSV(os.Stdout, result); err != nil {
			fmt.Fprintf(os.Stderr, "api-audit: write CSV: %v\n", err)
			os.Exit(1)
		}
	}
}

// findRepoRoot walks up from the current working directory looking for go.mod.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found in any parent directory")
		}
		dir = parent
	}
}

func printSummary(out io.Writer, result *validation.APIAuditResult, mesheryProvided, cloudProvided bool) {
	s := result.Summary
	consumerOnly := 0
	if result.Match != nil {
		consumerOnly = len(result.Match.ConsumerOnly)
	}
	fmt.Fprintln(out, "api-audit: scanning schemas...")
	fmt.Fprintf(out, "  found %d schema-defined endpoints (+ %d consumer-only handlers = %d audit rows)\n",
		s.SchemaEndpoints, consumerOnly, len(result.Rows))

	if mesheryProvided {
		fmt.Fprintf(out, "\napi-audit: scanning meshery/meshery...\n")
		fmt.Fprintf(out, "  parsed %d Gorilla route registrations\n", s.MesheryEndpoints)
	}
	if cloudProvided {
		fmt.Fprintf(out, "\napi-audit: scanning meshery-cloud...\n")
		fmt.Fprintf(out, "  parsed %d Echo route registrations\n", s.CloudEndpoints)
	}

	fmt.Fprintln(out, "\napi-audit: matching...")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "+---------------------------------+----------+----------+----------+")
	fmt.Fprintln(out, "|                                 |  Schema  | Meshery  |  Cloud   |")
	fmt.Fprintln(out, "+---------------------------------+----------+----------+----------+")
	fmt.Fprintf(out, "| %-31s | %8d | %8d | %8d |\n", "Total endpoints", s.SchemaEndpoints, s.MesheryEndpoints, s.CloudEndpoints)
	fmt.Fprintf(out, "| %-31s | %8d | %8s | %8s |\n", "Matched (schema <-> consumer)", s.Matched, "--", "--")
	fmt.Fprintf(out, "| %-31s | %8d | %8s | %8s |\n", "Schema-only (no handler)", s.SchemaOnly, "--", "--")
	fmt.Fprintf(out, "| %-31s | %8s | %8d | %8d |\n", "Consumer-only (no schema)", "--", consumerOnlyForRepo(result, "meshery"), consumerOnlyForRepo(result, "meshery-cloud"))
	fmt.Fprintln(out, "+---------------------------------+----------+----------+----------+")
	fmt.Fprintf(out, "| %-31s | %8s | %8d | %8d |\n", "Schema-Backed = TRUE", "--", s.MesheryBackedTrue, s.CloudBackedTrue)
	fmt.Fprintf(out, "| %-31s | %8s | %8d | %8d |\n", "Schema-Driven = TRUE", "--", s.MesheryDrivenTrue, s.CloudDrivenTrue)
	fmt.Fprintf(out, "| %-31s | %8s | %8d | %8d |\n", "Schema-Driven = Partial", "--", s.MesheryDrivenPartial, s.CloudDrivenPartial)
	fmt.Fprintf(out, "| %-31s | %8s | %8d | %8d |\n", "Schema-Driven = FALSE", "--", s.MesheryDrivenFalse, s.CloudDrivenFalse)
	fmt.Fprintf(out, "| %-31s | %8s | %8d | %8d |\n", "Schema-Driven = Not Audited", "--", s.MesheryDrivenNotAud, s.CloudDrivenNotAud)
	fmt.Fprintln(out, "+---------------------------------+----------+----------+----------+")
}

func consumerOnlyForRepo(result *validation.APIAuditResult, repo string) int {
	if result == nil || result.Match == nil {
		return 0
	}
	count := 0
	for _, c := range result.Match.ConsumerOnly {
		if c.Repo == repo {
			count++
		}
	}
	return count
}

func printVerbose(out io.Writer, result *validation.APIAuditResult) {
	if result == nil || result.Match == nil {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Schema-only endpoints (defined but no handler):")
	for _, ep := range result.Match.SchemaOnly {
		fmt.Fprintf(out, "  %-7s %s   (%s)\n", ep.Method, ep.Path, ep.SourceFile)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Consumer-only endpoints (registered but no schema):")
	for _, c := range result.Match.ConsumerOnly {
		fmt.Fprintf(out, "  %-7s %s   (%s, %s)\n", c.Method, c.Path, c.Repo, c.HandlerName)
	}
}

// printDiff prints a short summary of the reconciliation state transitions.
// Only runs when a previous state (sheet or cache) was available.
func printDiff(out io.Writer, tracked []validation.TrackedEndpoint) {
	type bucket struct {
		label string
		rows  []validation.TrackedEndpoint
	}
	buckets := map[validation.EndpointState]*bucket{
		validation.StateNew:     {label: "Added"},
		validation.StateChanged: {label: "Changed"},
		validation.StateDeleted: {label: "Removed"},
	}
	for _, t := range tracked {
		if b, ok := buckets[t.State]; ok {
			b.rows = append(b.rows, t)
		}
	}
	order := []validation.EndpointState{validation.StateNew, validation.StateChanged, validation.StateDeleted}
	anyChanges := false
	for _, st := range order {
		if len(buckets[st].rows) > 0 {
			anyChanges = true
			break
		}
	}
	fmt.Fprintln(out)
	if !anyChanges {
		fmt.Fprintln(out, "api-audit: no changes since last run")
		return
	}
	fmt.Fprintln(out, "api-audit: diff against previous state")
	for _, st := range order {
		b := buckets[st]
		if len(b.rows) == 0 {
			continue
		}
		sort.Slice(b.rows, func(i, j int) bool {
			if b.rows[i].Row.Endpoint != b.rows[j].Row.Endpoint {
				return b.rows[i].Row.Endpoint < b.rows[j].Row.Endpoint
			}
			return b.rows[i].Row.Method < b.rows[j].Row.Method
		})
		fmt.Fprintf(out, "  %s (%d):\n", b.label, len(b.rows))
		for _, t := range b.rows {
			fmt.Fprintf(out, "    %-7s %s  %s\n", t.Row.Method, t.Row.Endpoint, t.ChangeLog)
		}
	}
}

// writeCSV emits the full audit result (header + rows) as CSV to the given
// writer. When reconciliation has run, the reconciled rows are preferred so
// the emitted Change Log column reflects the state transitions.
func writeCSV(out io.Writer, result *validation.APIAuditResult) error {
	rows := result.CSVRows()
	w := csv.NewWriter(out)
	if err := w.WriteAll(rows); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

// readCSVCache loads a previously written .api-audit-cache.csv. A missing
// file is not an error — the first dry-run in a clean checkout legitimately
// has no previous state.
func readCSVCache(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	return r.ReadAll()
}

// writeCSVCache persists the reconciled state to disk for subsequent dry-run
// diffs. It is called after a successful sheet write; dry-runs never write
// the cache (otherwise a no-sheet run would poison the baseline).
func writeCSVCache(path string, result *validation.APIAuditResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeCSV(f, result)
}
