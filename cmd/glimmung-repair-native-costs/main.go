package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/nelsong6/glimmung/internal/domain/nativecostrepair"
	"github.com/nelsong6/glimmung/internal/server"
	pgstore "github.com/nelsong6/glimmung/internal/store/pg"
)

var errNoChanges = errors.New("no cost repair changes")

type repairOutput struct {
	Project          string                  `json:"project"`
	RunID            string                  `json:"run_id"`
	RunDisplayNumber string                  `json:"run_display_number"`
	IssueNumber      int                     `json:"issue_number,omitempty"`
	Applied          bool                    `json:"applied"`
	Result           nativecostrepair.Result `json:"result"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var (
		project          = flag.String("project", "", "project name")
		issueNumber      = flag.Int("issue-number", 0, "project-scoped issue number used with --run-display-number")
		runDisplayNumber = flag.String("run-display-number", "", "issue-scoped run display number, such as 4.2")
		runID            = flag.String("run-id", "", "durable run id; bypasses --issue-number/--run-display-number lookup")
		apply            = flag.Bool("apply", false, "write the repaired cost ledger; default is dry-run")
		timeout          = flag.Duration("timeout", 60*time.Second, "operation timeout")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if strings.TrimSpace(*project) == "" {
		return fmt.Errorf("--project is required")
	}
	if strings.TrimSpace(*runID) == "" && (*issueNumber <= 0 || strings.TrimSpace(*runDisplayNumber) == "") {
		return fmt.Errorf("provide either --run-id or both --issue-number and --run-display-number")
	}

	pool, err := openPostgres(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	runs := pgstore.NewRunsStore(pool)
	runEvents := pgstore.NewRunEventsStore(pool)
	row, payload, display, err := resolveRun(ctx, runs, *project, *runID, *issueNumber, *runDisplayNumber)
	if err != nil {
		return err
	}
	events, err := repairEvents(ctx, runEvents, row.ID)
	if err != nil {
		return err
	}

	var result nativecostrepair.Result
	applied := false
	if *apply {
		_, err = runs.PatchPayload(ctx, row.Project, row.ID, func(raw map[string]any) error {
			var repairErr error
			result, repairErr = nativecostrepair.RepairRunPayload(raw, events)
			if repairErr != nil {
				return repairErr
			}
			if !result.Changed {
				return errNoChanges
			}
			raw["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			return nil
		})
		if errors.Is(err, errNoChanges) {
			err = nil
		}
		if err != nil {
			return err
		}
		applied = result.Changed
	} else {
		result, err = nativecostrepair.RepairRunPayload(payload, events)
		if err != nil {
			return err
		}
	}

	out := repairOutput{
		Project:          row.Project,
		RunID:            row.ID,
		RunDisplayNumber: display,
		IssueNumber:      *issueNumber,
		Applied:          applied,
		Result:           result,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func openPostgres(ctx context.Context) (*pgstore.Pool, error) {
	settings := server.SettingsFromEnv()
	if settings.PostgresHost == "" || settings.PostgresDatabase == "" || settings.PostgresUsername == "" {
		return nil, fmt.Errorf("POSTGRES_HOST, POSTGRES_DATABASE, and POSTGRES_USER are required")
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("build default azure credential: %w", err)
	}
	return pgstore.NewPool(ctx, pgstore.Config{
		Host:       settings.PostgresHost,
		Database:   settings.PostgresDatabase,
		Username:   settings.PostgresUsername,
		Credential: cred,
	})
}

func resolveRun(ctx context.Context, runs *pgstore.RunsStore, project, runID string, issueNumber int, runDisplay string) (pgstore.RunRow, map[string]any, string, error) {
	if strings.TrimSpace(runID) != "" {
		row, err := runs.Get(ctx, project, strings.TrimSpace(runID))
		if err != nil {
			return pgstore.RunRow{}, nil, "", err
		}
		payload, err := decodePayload(row.Payload)
		if err != nil {
			return pgstore.RunRow{}, nil, "", err
		}
		return row, payload, displayNumber(payload), nil
	}
	rows, err := runs.ListByIssue(ctx, project, issueNumber)
	if err != nil {
		return pgstore.RunRow{}, nil, "", err
	}
	want := strings.TrimSpace(runDisplay)
	for _, row := range rows {
		payload, err := decodePayload(row.Payload)
		if err != nil {
			return pgstore.RunRow{}, nil, "", err
		}
		display := displayNumber(payload)
		if display == want {
			return row, payload, display, nil
		}
	}
	return pgstore.RunRow{}, nil, "", fmt.Errorf("run %s#%d/runs/%s not found", project, issueNumber, want)
}

func decodePayload(raw []byte) (map[string]any, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode run payload: %w", err)
	}
	return payload, nil
}

func displayNumber(payload map[string]any) string {
	if s, ok := payload["run_display_number"].(string); ok && s != "" {
		return s
	}
	runNumber, hasRun := numericInt(payload["run_number"])
	cycleNumber, hasCycle := numericInt(payload["run_cycle_number"])
	if hasRun && hasCycle && cycleNumber > 0 {
		return fmt.Sprintf("%d.%d", runNumber, cycleNumber)
	}
	if hasRun {
		return strconv.Itoa(runNumber)
	}
	return ""
}

func numericInt(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	default:
		return 0, false
	}
}

func repairEvents(ctx context.Context, store *pgstore.RunEventsStore, runID string) ([]nativecostrepair.Event, error) {
	rows, err := store.List(ctx, runID, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	events := make([]nativecostrepair.Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, nativecostrepair.Event{
			AttemptIndex: row.AttemptIndex,
			JobID:        row.JobID,
			Event:        row.Event,
			Message:      row.Message,
		})
	}
	return events, nil
}
