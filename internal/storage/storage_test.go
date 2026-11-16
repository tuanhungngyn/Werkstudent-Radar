package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"werkstudent-radar/internal/model"
)

// Fixed times instead of time.Now(), so tests are exact and repeatable.
var (
	t1 = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	t2 = t1.Add(2 * time.Hour)
)

// openTestStorage opens a fresh database in a temporary folder that Go deletes
// after the test. It returns the storage and the file path (for reopen tests).
func openTestStorage(t *testing.T) (*Storage, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// job builds a minimal valid job.
func job(source, ref, title string) model.Job {
	return model.Job{Source: source, ReferenceNumber: ref, Title: title}
}

// mustSave calls Save and fails the test on an error.
func mustSave(t *testing.T, s *Storage, jobs []model.Job, now time.Time) []model.Job {
	t.Helper()
	newJobs, err := s.Save(context.Background(), jobs, now)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return newJobs
}

// refs returns the reference numbers of jobs, to compare results easily.
func refs(jobs []model.Job) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.Source+"/"+j.ReferenceNumber)
	}
	return out
}

func assertRefs(t *testing.T, got []model.Job, want ...string) {
	t.Helper()
	g := refs(got)
	if len(g) != len(want) {
		t.Fatalf("new jobs = %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("new jobs = %v, want %v", g, want)
		}
	}
}

// row reads selected columns of one stored job. The test is in package
// storage, so it may use s.db directly; no public getter needed.
type row struct {
	title, description, firstSeen, lastSeen string
}

func readRow(t *testing.T, s *Storage, source, ref string) row {
	t.Helper()
	var r row
	err := s.db.QueryRow(
		`SELECT title, description, first_seen, last_seen FROM jobs WHERE source = ? AND ref = ?`,
		source, ref,
	).Scan(&r.title, &r.description, &r.firstSeen, &r.lastSeen)
	if err != nil {
		t.Fatalf("read %s/%s: %v", source, ref, err)
	}
	return r
}

func countJobs(t *testing.T, s *Storage) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// ---------------------------------------------------------------- Open

func TestOpenSetsUpDatabase(t *testing.T) {
	s, _ := openTestStorage(t)

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}

	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q (DSN settings not applied?)", mode, "wal")
	}
}

func TestOpenInvalidPathFails(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "does-not-exist", "test.db"))
	if err == nil {
		t.Fatal("expected an error for a folder that does not exist, got nil")
	}
}

func TestOpenTwiceKeepsData(t *testing.T) {
	s, path := openTestStorage(t)
	mustSave(t, s, []model.Job{job("aa", "1", "A"), job("aa", "2", "B")}, t1)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening runs the schema again: IF NOT EXISTS must keep the data.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	if n := countJobs(t, s2); n != 2 {
		t.Errorf("after reopen: %d jobs, want 2", n)
	}
	// And they are known, not new.
	newJobs, err := s2.Save(context.Background(), []model.Job{job("aa", "1", "A")}, t2)
	if err != nil {
		t.Fatal(err)
	}
	assertRefs(t, newJobs)
}

// ---------------------------------------------------------------- Save: new vs known

func TestSaveFirstTimeAllNew(t *testing.T) {
	s, _ := openTestStorage(t)
	got := mustSave(t, s, []model.Job{job("aa", "1", "A"), job("aa", "2", "B"), job("aa", "3", "C")}, t1)
	assertRefs(t, got, "aa/1", "aa/2", "aa/3")
}

func TestSaveAgainNothingNew(t *testing.T) {
	s, _ := openTestStorage(t)
	jobs := []model.Job{job("aa", "1", "A"), job("aa", "2", "B"), job("aa", "3", "C")}
	mustSave(t, s, jobs, t1)

	got := mustSave(t, s, jobs, t2)
	assertRefs(t, got)
}

func TestSaveMixedOldAndNew(t *testing.T) {
	s, _ := openTestStorage(t)
	mustSave(t, s, []model.Job{job("aa", "1", "A"), job("aa", "2", "B"), job("aa", "3", "C")}, t1)

	got := mustSave(t, s, []model.Job{
		job("aa", "1", "A"), job("aa", "4", "D"), job("aa", "2", "B"), job("aa", "5", "E"),
	}, t2)
	assertRefs(t, got, "aa/4", "aa/5")
}

func TestSaveDuplicateInSameRunCountsOnce(t *testing.T) {
	// Happens for real when listings shift between pages during pagination.
	s, _ := openTestStorage(t)
	got := mustSave(t, s, []model.Job{job("aa", "1", "A"), job("aa", "2", "B"), job("aa", "1", "A")}, t1)
	assertRefs(t, got, "aa/1", "aa/2")

	if n := countJobs(t, s); n != 2 {
		t.Errorf("%d rows stored, want 2", n)
	}
}

func TestSaveSameRefDifferentSource(t *testing.T) {
	s, _ := openTestStorage(t)
	mustSave(t, s, []model.Job{job("aa", "1", "from aa")}, t1)

	got := mustSave(t, s, []model.Job{job("other", "1", "from other")}, t2)
	assertRefs(t, got, "other/1")

	if a, o := readRow(t, s, "aa", "1").title, readRow(t, s, "other", "1").title; a != "from aa" || o != "from other" {
		t.Errorf("titles = %q / %q, the two jobs must stay separate", a, o)
	}
}

func TestSaveEmptyList(t *testing.T) {
	s, _ := openTestStorage(t)
	got := mustSave(t, s, nil, t1)
	assertRefs(t, got)
}

// ---------------------------------------------------------------- Save: what gets stored

func TestSaveUpdatesLastSeenKeepsFirstSeen(t *testing.T) {
	s, _ := openTestStorage(t)
	mustSave(t, s, []model.Job{job("aa", "1", "A")}, t1)
	mustSave(t, s, []model.Job{job("aa", "1", "A")}, t2)

	r := readRow(t, s, "aa", "1")
	if want := t1.Format(time.RFC3339); r.firstSeen != want {
		t.Errorf("first_seen = %s, want %s", r.firstSeen, want)
	}
	if want := t2.Format(time.RFC3339); r.lastSeen != want {
		t.Errorf("last_seen = %s, want %s", r.lastSeen, want)
	}
}

func TestSaveStoresTimesInUTC(t *testing.T) {
	s, _ := openTestStorage(t)
	berlin := time.FixedZone("CET", 60*60)
	local := time.Date(2026, 11, 16, 11, 0, 0, 0, berlin) // = 10:00 UTC

	mustSave(t, s, []model.Job{job("aa", "1", "A")}, local)

	if got, want := readRow(t, s, "aa", "1").firstSeen, "2026-11-16T10:00:00Z"; got != want {
		t.Errorf("first_seen = %s, want %s", got, want)
	}
}

func TestSaveUpdatesChangedTitle(t *testing.T) {
	s, _ := openTestStorage(t)
	mustSave(t, s, []model.Job{job("aa", "1", "Werkstudent Backend")}, t1)

	got := mustSave(t, s, []model.Job{job("aa", "1", "Werkstudent Backend (Go)")}, t2)
	assertRefs(t, got) // a renamed job is still the same job, not new

	if title := readRow(t, s, "aa", "1").title; title != "Werkstudent Backend (Go)" {
		t.Errorf("title = %q, want the updated title", title)
	}
}

func TestSaveKeepsFetchedDescription(t *testing.T) {
	s, _ := openTestStorage(t)
	mustSave(t, s, []model.Job{job("aa", "1", "A")}, t1)

	// Simulates the later details step writing a description.
	if _, err := s.db.Exec(`UPDATE jobs SET description = 'fetched text' WHERE source = 'aa' AND ref = '1'`); err != nil {
		t.Fatal(err)
	}

	// The next search run has no description (search results never do).
	mustSave(t, s, []model.Job{job("aa", "1", "A")}, t2)

	if d := readRow(t, s, "aa", "1").description; d != "fetched text" {
		t.Errorf("description = %q, a later Save must not wipe it", d)
	}
}

// ---------------------------------------------------------------- Save: errors

func TestSaveMissingKey(t *testing.T) {
	tests := []struct {
		name string
		job  model.Job
	}{
		{"no source", model.Job{ReferenceNumber: "1", Title: "A"}},
		{"no reference number", model.Job{Source: "aa", Title: "A"}},
		{"neither", model.Job{Title: "A"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := openTestStorage(t)
			_, err := s.Save(context.Background(), []model.Job{tt.job}, t1)
			if !errors.Is(err, ErrMissingKey) {
				t.Errorf("err = %v, want ErrMissingKey", err)
			}
		})
	}
}

func TestSaveErrorRollsBackWholeRun(t *testing.T) {
	s, _ := openTestStorage(t)

	// Two valid jobs, then a broken one: nothing of this run may be stored.
	_, err := s.Save(context.Background(), []model.Job{
		job("aa", "1", "A"), job("aa", "2", "B"), {Title: "broken"},
	}, t1)
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := countJobs(t, s); n != 0 {
		t.Errorf("%d rows stored after a failed Save, want 0 (transaction not rolled back)", n)
	}
}

func TestSaveCancelledContext(t *testing.T) {
	s, _ := openTestStorage(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Save starts

	if _, err := s.Save(ctx, []model.Job{job("aa", "1", "A")}, t1); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
	if n := countJobs(t, s); n != 0 {
		t.Errorf("%d rows stored with a cancelled context, want 0", n)
	}
}
