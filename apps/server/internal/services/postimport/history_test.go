package postimport

import (
	"context"
	"testing"
	"time"

	"github.com/openpost/backend/internal/models"
	"github.com/openpost/backend/internal/platform"
	"github.com/stretchr/testify/require"
)

type historyReader struct {
	*platform.FacebookAdapter
	requests  []platform.NativePostRequest
	pages     map[string]platform.NativePostPage
	afterRead func()
}

func (r *historyReader) ListNativePosts(_ context.Context, _ string, request platform.NativePostRequest) (platform.NativePostPage, error) {
	r.requests = append(r.requests, request)
	if r.afterRead != nil {
		r.afterRead()
	}
	return r.pages[request.Cursor], nil
}

func TestHistoricalImportPauseResumeAndDeduplication(t *testing.T) {
	for _, all := range []bool{true, false} {
		t.Run(map[bool]string{true: "all", false: "start_date"}[all], func(t *testing.T) {
			ctx := t.Context()
			db := newPostImportTestDB(t)
			account := seedPostImportAccount(t, db, "facebook", "page-1", "")
			_, err := db.NewUpdate().Model(&account).Set("granted_scopes = ?", "pages_read_engagement").WherePK().Exec(ctx)
			require.NoError(t, err)
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			start := now.AddDate(-2, 0, 0)
			old := platform.NativePostItem{ProviderPostID: "old", Text: "history", ExternalURL: "https://www.facebook.com/page-1/posts/old", PublishedAt: start, Origin: platform.ImportedPostOriginExternal}
			older := old
			older.ProviderPostID = "older"
			older.PublishedAt = start.Add(-time.Hour)
			reader := &historyReader{FacebookAdapter: platform.NewFacebookAdapter("", "", ""), pages: map[string]platform.NativePostPage{
				"":     {Items: []platform.NativePostItem{old, older}, NextCursor: "next", Coverage: platform.NativePostPartial},
				"next": {Items: []platform.NativePostItem{old}, Coverage: platform.NativePostComplete},
			}}
			service := NewService(db, &stubTokenSource{token: "fixture"})
			service.SetProvider("facebook", reader)
			service.now = func() time.Time { return now }
			service.maxPages = 1
			_, err = service.Enable(ctx, account.WorkspaceID, account.ID)
			require.NoError(t, err)
			// Leave an existing ongoing checkpoint in place, not due for a read.
			_, err = db.NewUpdate().Model((*models.PostImportState)(nil)).Set("cursor = ?", "ongoing-cursor").Set("next_eligible_at = ?", now.AddDate(0, 0, 30)).Where("social_account_id = ?", account.ID).Exec(ctx)
			require.NoError(t, err)
			before := loadImportState(t, db, account.ID)
			var date *time.Time
			if !all {
				date = &start
			}
			require.NoError(t, service.SetHistoricalImport(ctx, account.WorkspaceID, account.ID, "start", date))
			require.NoError(t, service.SyncAccount(ctx, account.WorkspaceID, account.ID))
			require.Len(t, reader.requests, 1)
			if all {
				require.True(t, reader.requests[0].PublishedAfter.IsZero())
			} else {
				require.True(t, reader.requests[0].PublishedAfter.Equal(start.Add(-time.Nanosecond)))
			}
			state := loadImportState(t, db, account.ID)
			require.Equal(t, before.Cursor, state.Cursor)
			require.True(t, before.ImportWatermark.Equal(state.ImportWatermark))
			require.True(t, before.NextEligibleAt.Equal(state.NextEligibleAt))
			history, err := historicalState(&state)
			require.NoError(t, err)
			require.Equal(t, "next", history.Cursor)
			require.NoError(t, service.SetHistoricalImport(ctx, account.WorkspaceID, account.ID, "pause", nil))
			now = now.Add(25 * time.Hour)
			require.NoError(t, service.SyncAccount(ctx, account.WorkspaceID, account.ID))
			require.Len(t, reader.requests, 1)
			require.NoError(t, service.SetHistoricalImport(ctx, account.WorkspaceID, account.ID, "resume", nil))
			// Simulate process restart: all progress comes from the database.
			restarted := NewService(db, &stubTokenSource{token: "fixture"})
			restarted.SetProvider("facebook", reader)
			restarted.now = service.now
			require.NoError(t, restarted.SyncAccount(ctx, account.WorkspaceID, account.ID))
			require.Len(t, reader.requests, 2)
			require.Equal(t, "next", reader.requests[1].Cursor)
			want := 1
			if all {
				want = 2
			}
			require.Equal(t, want, countImported(t, db, account.ID))
			state = loadImportState(t, db, account.ID)
			require.False(t, state.HistoryEnabled)
			status, err := ReadHistoricalStatus(&state)
			require.NoError(t, err)
			require.NotNil(t, status.FinishedAt)
			require.NoError(t, restarted.SyncAccount(ctx, account.WorkspaceID, account.ID))
			require.Len(t, reader.requests, 2)
			require.ErrorIs(t, service.SetHistoricalImport(ctx, account.WorkspaceID, account.ID, "start", nil), ErrHistoricalChoice)
			posts, err := service.ListImported(ctx, account.WorkspaceID, account.ID, 50)
			require.NoError(t, err)
			require.Len(t, posts, want)
			for _, post := range posts {
				require.Equal(t, platform.ImportedPostOriginExternal, post.Origin)
			}
		})
	}
}

func TestHistoricalImportPermissionAndInFlightPause(t *testing.T) {
	ctx := t.Context()
	db := newPostImportTestDB(t)
	account := seedPostImportAccount(t, db, "facebook", "page-1", "")
	reader := &historyReader{FacebookAdapter: platform.NewFacebookAdapter("", "", ""), pages: map[string]platform.NativePostPage{
		"": {Items: []platform.NativePostItem{{ProviderPostID: "old", PublishedAt: time.Now().AddDate(-1, 0, 0)}}, Coverage: platform.NativePostComplete},
	}}
	service := NewService(db, &stubTokenSource{token: "fixture"})
	service.SetProvider("facebook", reader)
	require.ErrorContains(t, service.SetHistoricalImport(ctx, account.WorkspaceID, account.ID, "start", nil), "pages_read_engagement")
	require.Empty(t, reader.requests)
	_, err := db.NewUpdate().Model(&account).Set("granted_scopes = ?", "pages_read_engagement").WherePK().Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, service.SetHistoricalImport(ctx, account.WorkspaceID, account.ID, "start", nil))
	reader.afterRead = func() {
		require.NoError(t, service.SetHistoricalImport(ctx, account.WorkspaceID, account.ID, "pause", nil))
	}
	require.NoError(t, service.SyncAccount(ctx, account.WorkspaceID, account.ID))
	require.Zero(t, countImported(t, db, account.ID))
	state := loadImportState(t, db, account.ID)
	require.False(t, state.HistoryEnabled)
}

func TestHistoricalImportSharesBudgetAndResumesNextUTCDay(t *testing.T) {
	ctx := t.Context()
	db := newPostImportTestDB(t)
	account := seedPostImportAccount(t, db, "facebook", "page-1", "")
	_, err := db.NewUpdate().Model(&account).Set("granted_scopes = ?", "pages_read_engagement").WherePK().Exec(ctx)
	require.NoError(t, err)
	now := time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC)
	reader := &historyReader{FacebookAdapter: platform.NewFacebookAdapter("", "", ""), pages: map[string]platform.NativePostPage{
		"":     {NextCursor: "next", Coverage: platform.NativePostPartial},
		"next": {Coverage: platform.NativePostComplete},
	}}
	service := NewService(db, &stubTokenSource{token: "fixture"})
	service.SetProvider("facebook", reader)
	service.now = func() time.Time { return now }
	service.SetPolicy("facebook", Policy{ReadRequestsPerDay: 1, PageSize: 50})
	require.NoError(t, service.SetHistoricalImport(ctx, account.WorkspaceID, account.ID, "start", nil))
	require.NoError(t, service.SyncAccount(ctx, account.WorkspaceID, account.ID))
	state := loadImportState(t, db, account.ID)
	require.False(t, state.Enabled)
	require.Equal(t, 1, state.ReadBudgetUsed)
	history, err := historicalState(&state)
	require.NoError(t, err)
	require.Equal(t, "next", history.Cursor)
	require.Equal(t, string(platform.NativePostCostLimited), history.Status)
	require.True(t, history.NextEligibleAt.Equal(nextUTCDay(now)))
	_, err = db.NewUpdate().Model((*models.Job)(nil)).Set("status = ?", "completed").Where("dedupe_key = ?", account.ID).Exec(ctx)
	require.NoError(t, err)
	queued, err := service.EnqueueDue(ctx)
	require.NoError(t, err)
	require.Zero(t, queued, "a budget-limited historical scan is not due yet")
	require.NoError(t, service.SetHistoricalImport(ctx, account.WorkspaceID, account.ID, "pause", nil))
	require.NoError(t, service.SetHistoricalImport(ctx, account.WorkspaceID, account.ID, "resume", nil))
	require.NoError(t, service.SyncAccount(ctx, account.WorkspaceID, account.ID))
	require.Len(t, reader.requests, 1)
	now = nextUTCDay(now)
	_, err = db.NewUpdate().Model((*models.Job)(nil)).Set("status = ?", "completed").Where("dedupe_key = ?", account.ID).Exec(ctx)
	require.NoError(t, err)
	queued, err = service.EnqueueDue(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, queued, "the existing worker sweep recovers historical work")
	require.NoError(t, service.SyncAccount(ctx, account.WorkspaceID, account.ID))
	require.Len(t, reader.requests, 2)
	require.Equal(t, "next", reader.requests[1].Cursor)
	state = loadImportState(t, db, account.ID)
	require.Equal(t, 1, state.ReadBudgetUsed)
	require.False(t, state.HistoryEnabled)
}
