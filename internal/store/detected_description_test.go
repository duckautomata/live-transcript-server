package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"live-transcript-server/internal/model"
)

// A description is whatever the creator typed: several lines, a blank one,
// links. The ledger hands it back byte for byte through every getter, since
// previews and test sends render from whichever getter found the row.
func TestDetectionDescriptionRoundTrips(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const desc = "First line of the pitch\n\nhttps://example.test/merch\n  indented credit\n"

	d := newDetection("youtube", "vid1", "doki")
	d.Description = desc
	if won, err := st.ClaimDetection(ctx, d); err != nil || !won {
		t.Fatalf("claim = (%v, %v), want (true, nil)", won, err)
	}

	got, err := st.GetDetection(ctx, "youtube", "vid1")
	if err != nil || got == nil {
		t.Fatalf("GetDetection = (%+v, %v)", got, err)
	}
	if got.Description != desc {
		t.Errorf("GetDetection description = %q, want %q", got.Description, desc)
	}

	live, err := st.GetLiveDetections(ctx)
	if err != nil || len(live) != 1 {
		t.Fatalf("GetLiveDetections = (%+v, %v), want one row", live, err)
	}
	if live[0].Description != desc {
		t.Errorf("GetLiveDetections description = %q, want %q", live[0].Description, desc)
	}

	recent, err := st.GetRecentDetections(ctx, "doki", 10)
	if err != nil || len(recent) != 1 {
		t.Fatalf("GetRecentDetections = (%+v, %v), want one row", recent, err)
	}
	if recent[0].Description != desc {
		t.Errorf("GetRecentDetections description = %q, want %q", recent[0].Description, desc)
	}

	// A row claimed without one - every Twitch row - reads back empty.
	if won, err := st.ClaimDetection(ctx, newDetection("twitch", "s1", "doki")); err != nil || !won {
		t.Fatalf("twitch claim = (%v, %v)", won, err)
	}
	if got, _ := st.GetDetection(ctx, "twitch", "s1"); got == nil || got.Description != "" {
		t.Errorf("a row claimed with no description = %+v, want it empty", got)
	}
}

// The video ledger's claim is an INSERT ... SELECT whose arguments come in
// two groups (the values, then the sibling guard), so this also proves the
// description landed in the first group: a misplaced argument would store it
// in another column or break the guard.
func TestVideoDetectionDescriptionRoundTrips(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const desc = "What the video is about\n\nMore below:\nhttps://example.test/a"

	v := model.DetectedVideo{
		Platform: "youtube", VideoID: "up1", Kind: "upload", ChannelKey: "doki",
		URL: "https://example.test/up1", Title: "an upload", Description: desc,
		PublishedAt: 900, ScheduledAt: 0, DetectedAt: 1000,
	}
	if won, err := st.ClaimVideoDetection(ctx, v); err != nil || !won {
		t.Fatalf("claim = (%v, %v), want (true, nil)", won, err)
	}

	vids, err := st.GetRecentVideoDetections(ctx, "doki", 10)
	if err != nil || len(vids) != 1 {
		t.Fatalf("GetRecentVideoDetections = (%+v, %v), want one row", vids, err)
	}
	if vids[0] != v {
		t.Errorf("row read back = %+v, want %+v", vids[0], v)
	}

	// The guard still works: the same video classified the other way loses.
	sibling := v
	sibling.Kind = "short"
	if won, err := st.ClaimVideoDetection(ctx, sibling); err != nil || won {
		t.Errorf("sibling claim = (%v, %v), want (false, nil)", won, err)
	}
}

// hasColumn reports whether a table has a column, read the way ensureColumn
// reads it.
func hasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("table_info(%s): %v", table, err)
		}
		if name == column {
			return true
		}
	}
	return false
}

// The deployed database already has both ledgers, so CREATE TABLE IF NOT
// EXISTS does nothing there and the new column has to be added. Without that
// every claim fails with "no such column" and detection goes silent. Built
// from the tables exactly as they shipped, migrated twice (every boot runs the
// schema), and then used.
func TestSchemaAddsDescriptionToExistingLedgers(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, ddl := range []string{`
	CREATE TABLE detected_broadcasts (
		platform TEXT NOT NULL,
		broadcast_id TEXT NOT NULL,
		channel_key TEXT NOT NULL,
		url TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		started_at INTEGER NOT NULL DEFAULT 0,
		detected_at INTEGER NOT NULL,
		mechanism TEXT NOT NULL DEFAULT '',
		ended_at INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (platform, broadcast_id)
	);`, `
	CREATE TABLE detected_videos (
		platform TEXT NOT NULL,
		video_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		channel_key TEXT NOT NULL,
		url TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		published_at INTEGER NOT NULL DEFAULT 0,
		scheduled_at INTEGER NOT NULL DEFAULT 0,
		detected_at INTEGER NOT NULL,
		PRIMARY KEY (platform, video_id, kind)
	);`, `
	INSERT INTO detected_broadcasts (platform, broadcast_id, channel_key, url, title, started_at, detected_at, mechanism, ended_at)
	VALUES ('youtube', 'old-live', 'doki', 'https://example.test/old-live', 'from before', 1000, 1005, 'test', 0);`, `
	INSERT INTO detected_videos (platform, video_id, kind, channel_key, url, title, published_at, scheduled_at, detected_at)
	VALUES ('youtube', 'old-up', 'upload', 'doki', 'https://example.test/old-up', 'from before', 900, 0, 1000);`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("building the old database: %v", err)
		}
	}
	for _, table := range []string{"detected_broadcasts", "detected_videos"} {
		if hasColumn(t, db, table, "description") {
			t.Fatalf("%s already has the column; this is not the old shape", table)
		}
	}
	// The Twitch category came the same way, on the broadcast ledger only.
	if hasColumn(t, db, "detected_broadcasts", "game") {
		t.Fatal("detected_broadcasts already has the game column; this is not the old shape")
	}

	for boot := 1; boot <= 2; boot++ {
		if err := createSchema(db); err != nil {
			t.Fatalf("createSchema, boot %d: %v", boot, err)
		}
	}
	for _, table := range []string{"detected_broadcasts", "detected_videos"} {
		if !hasColumn(t, db, table, "description") {
			t.Errorf("%s did not gain the description column", table)
		}
	}
	if !hasColumn(t, db, "detected_broadcasts", "game") {
		t.Error("detected_broadcasts did not gain the game column")
	}

	st := &Store{db: db}
	ctx := context.Background()

	// Rows from before the columns read back with no description and no game.
	if got, err := st.GetDetection(ctx, "youtube", "old-live"); err != nil || got == nil || got.Title != "from before" || got.Description != "" || got.Game != "" {
		t.Errorf("old broadcast row = (%+v, %v), want it intact with an empty description and game", got, err)
	}
	vids, err := st.GetRecentVideoDetections(ctx, "doki", 10)
	if err != nil || len(vids) != 1 || vids[0].Title != "from before" || vids[0].Description != "" {
		t.Errorf("old video rows = (%+v, %v), want the one row intact with an empty description", vids, err)
	}

	// And new claims work against the migrated tables.
	d := newDetection("youtube", "new-live", "doki")
	d.Description = "fresh"
	if won, err := st.ClaimDetection(ctx, d); err != nil || !won {
		t.Fatalf("claim after migrating = (%v, %v), want (true, nil)", won, err)
	}
	if got, _ := st.GetDetection(ctx, "youtube", "new-live"); got == nil || got.Description != "fresh" {
		t.Errorf("new broadcast row = %+v", got)
	}
	tw := newDetection("twitch", "new-stream", "doki")
	tw.Game = "Minecraft"
	if won, err := st.ClaimDetection(ctx, tw); err != nil || !won {
		t.Fatalf("twitch claim after migrating = (%v, %v), want (true, nil)", won, err)
	}
	if got, _ := st.GetDetection(ctx, "twitch", "new-stream"); got == nil || got.Game != "Minecraft" {
		t.Errorf("new twitch row = %+v", got)
	}
	// The guarded fill matches a row from before the column: the added column
	// reads '' there, not NULL, which "AND game = ''" would never match.
	if filled, err := st.FillDetectionGame(ctx, "youtube", "old-live", "backfilled"); err != nil || !filled {
		t.Errorf("fill on a migrated row = (%v, %v), want (true, nil)", filled, err)
	}
	v := model.DetectedVideo{
		Platform: "youtube", VideoID: "new-up", Kind: "short", ChannelKey: "doki",
		URL: "https://example.test/new-up", Description: "fresh video", DetectedAt: 2000,
	}
	if won, err := st.ClaimVideoDetection(ctx, v); err != nil || !won {
		t.Fatalf("video claim after migrating = (%v, %v), want (true, nil)", won, err)
	}
	if vids, _ := st.GetRecentVideoDetections(ctx, "doki", 10); len(vids) != 2 || vids[0].Description != "fresh video" {
		t.Errorf("video rows after the new claim = %+v", vids)
	}
}
