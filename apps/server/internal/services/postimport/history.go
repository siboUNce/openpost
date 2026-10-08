package postimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/openpost/backend/internal/models"
	"github.com/openpost/backend/internal/platform"
	"github.com/uptrace/bun"
)

var ErrHistoricalChoice = errors.New("invalid historical import choice")

// The ongoing and historical checkpoints share only the account's read budget.
// Cursor and cycle time are private fields on PostImportState, so persist them
// explicitly inside the historical checkpoint.
type historicalCheckpoint struct {
	State          *models.PostImportState `json:"state"`
	Cursor         string                  `json:"cursor"`
	CycleStartedAt time.Time               `json:"cycle_started_at"`
}

type HistoricalStatus struct {
	Enabled        bool       `json:"enabled"`
	Status         string     `json:"status"`
	StartDate      *time.Time `json:"start_date,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	NextEligibleAt *time.Time `json:"next_eligible_at,omitempty"`
	FailureMessage string     `json:"failure_message,omitempty"`
}

func historicalState(state *models.PostImportState) (*models.PostImportState, error) {
	if state == nil || state.HistoryJSON == "" {
		return nil, nil
	}
	var checkpoint historicalCheckpoint
	if err := json.Unmarshal([]byte(state.HistoryJSON), &checkpoint); err != nil {
		return nil, fmt.Errorf("decode historical checkpoint: %w", err)
	}
	if checkpoint.State == nil {
		return nil, fmt.Errorf("missing historical checkpoint")
	}
	history := checkpoint.State
	history.ID, history.WorkspaceID, history.SocialAccountID, history.Platform = state.ID, state.WorkspaceID, state.SocialAccountID, state.Platform
	history.Cursor, history.CycleStartedAt = checkpoint.Cursor, checkpoint.CycleStartedAt
	history.Enabled, history.HistoryEnabled, history.Historical = state.HistoryEnabled, state.HistoryEnabled, true
	history.HistoryJSON = state.HistoryJSON
	history.ReadBudgetStart, history.ReadBudgetUsed = state.ReadBudgetStart, state.ReadBudgetUsed
	return history, nil
}

func ReadHistoricalStatus(state *models.PostImportState) (*HistoricalStatus, error) {
	history, err := historicalState(state)
	if err != nil || history == nil {
		return nil, err
	}
	status := &HistoricalStatus{Enabled: history.Enabled, Status: history.Status, FailureMessage: history.FailureMessage}
	if !history.ImportWatermark.IsZero() {
		start := history.ImportWatermark
		status.StartDate = &start
	}
	if !history.InitialFinishedAt.IsZero() {
		finished := history.InitialFinishedAt
		status.FinishedAt = &finished
	}
	if !history.NextEligibleAt.IsZero() {
		next := history.NextEligibleAt
		status.NextEligibleAt = &next
	}
	return status, nil
}

// SetHistoricalImport starts one Facebook history scan or pauses/resumes its
// existing checkpoint. A completed scan cannot be restarted or have its window
// changed. A nil start date means all history the API makes available.
func (s *Service) SetHistoricalImport(ctx context.Context, workspaceID, accountID, action string, startDate *time.Time) error {
	account, err := s.loadAccount(ctx, workspaceID, accountID)
	if err != nil {
		return err
	}
	if account.Platform != "facebook" {
		return fmt.Errorf("%w: history is supported for Facebook Pages only", ErrHistoricalChoice)
	}
	if action != "start" && action != "pause" && action != "resume" {
		return ErrHistoricalChoice
	}
	if startDate != nil && (action != "start" || startDate.IsZero() || startDate.After(s.now())) {
		return ErrHistoricalChoice
	}
	if action != "pause" {
		support := s.support(account)
		if !support.Supported {
			return fmt.Errorf("%w: %s", ErrHistoricalChoice, support.UnavailableReason)
		}
	}
	now := s.now().UTC()
	// Create an inactive ongoing row when history is the first import choice.
	// ON CONFLICT does nothing: existing checkpoints are never reset.
	if action == "start" {
		row := &models.PostImportState{ID: uuid.NewString(), WorkspaceID: account.WorkspaceID, SocialAccountID: account.ID, Platform: account.Platform, Enabled: false, CreatedAt: now, UpdatedAt: now}
		if _, err := s.db.NewInsert().Model(row).
			Column("id", "workspace_id", "social_account_id", "platform", "enabled", "created_at", "updated_at").
			Value("enabled", "?", false).On("CONFLICT (social_account_id) DO NOTHING").Exec(ctx); err != nil {
			return err
		}
	}
	state, err := s.loadState(ctx, account.ID)
	if err != nil {
		return err
	}
	if state == nil {
		return ErrHistoricalChoice
	}
	history, err := historicalState(state)
	if err != nil {
		return err
	}
	if action == "start" {
		if history != nil {
			return fmt.Errorf("%w: history already started; resume its existing checkpoint", ErrHistoricalChoice)
		}
		history = &models.PostImportState{Status: string(platform.NativePostPartial), CycleStartedAt: now, CreatedAt: now, UpdatedAt: now}
		if startDate != nil {
			history.ImportWatermark = startDate.UTC()
		}
	} else if history == nil || !history.InitialFinishedAt.IsZero() {
		return fmt.Errorf("%w: no unfinished history import", ErrHistoricalChoice)
	}
	payload, err := json.Marshal(historicalCheckpoint{State: history, Cursor: history.Cursor, CycleStartedAt: history.CycleStartedAt})
	if err != nil {
		return err
	}
	result, err := s.db.NewUpdate().Model((*models.PostImportState)(nil)).
		Set("history_json = ?", string(payload)).Set("history_enabled = ?", action != "pause").
		Set("history_next_eligible_at = ?", history.NextEligibleAt).
		Where("id = ? AND history_json = ? AND history_enabled = ?", state.ID, state.HistoryJSON, state.HistoryEnabled).Exec(ctx)
	if err := importUpdateResult(result, err); err != nil {
		return err
	}
	if action != "pause" {
		_, err = s.enqueueSync(ctx, workspaceID, accountID, now)
	}
	return err
}

// checkpointUpdate maps a historical run onto its private checkpoint while
// keeping normal progress intact. Comparing the stored JSON rejects in-flight
// work after a pause; normal runs exclude the history columns.
func checkpointUpdate(query *bun.UpdateQuery, state *models.PostImportState) (*bun.UpdateQuery, string, error) {
	if !state.Historical {
		return query.ExcludeColumn("history_enabled", "history_json", "history_next_eligible_at").Where("enabled = ? AND import_watermark = ?", true, state.ImportWatermark), "", nil
	}
	payload, err := json.Marshal(historicalCheckpoint{State: state, Cursor: state.Cursor, CycleStartedAt: state.CycleStartedAt})
	if err != nil {
		return nil, "", err
	}
	return query.
		Set("history_json = ?", string(payload)).Set("history_enabled = ?", state.InitialFinishedAt.IsZero()).
		Set("history_next_eligible_at = ?", state.NextEligibleAt).
		Set("read_budget_start = ?", state.ReadBudgetStart).Set("read_budget_used = ?", state.ReadBudgetUsed).
		Where("history_enabled = ? AND history_json = ?", true, state.HistoryJSON), string(payload), nil
}
