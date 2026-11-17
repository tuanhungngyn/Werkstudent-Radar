package radar

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"werkstudent-radar/internal/model"
	"werkstudent-radar/internal/storage"
)

var t1 = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------- fakes

// fakeCollector returns fixed jobs or a fixed error. Optional hooks let a test
// slow it down, block until the context ends, or inspect the context it got.
type fakeCollector struct {
	name    string
	jobs    []model.Job
	err     error
	delay   time.Duration             // sleep before answering
	block   bool                      // wait until ctx is cancelled
	onStart func(ctx context.Context) // called first, with the ctx it received
}

func (f *fakeCollector) Name() string { return f.name }

func (f *fakeCollector) Collect(ctx context.Context, q model.Query) ([]model.Job, error) {
	if f.onStart != nil {
		f.onStart(ctx)
	}
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.jobs, f.err
}

// fakeSaver records what it was given and can fail on demand.
type fakeSaver struct {
	got []model.Job
	err error
}

func (s *fakeSaver) Save(ctx context.Context, jobs []model.Job, now time.Time) ([]model.Job, error) {
	s.got = jobs
	if s.err != nil {
		return nil, s.err
	}
	return jobs, nil
}

// ---------------------------------------------------------------- helpers

// openStorage gives a test a real SQLite database in a temporary folder.
func openStorage(t *testing.T) *storage.Storage {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "radar.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

// jobs builds jobs with the given reference numbers (no Source: the radar sets it).
func jobs(refs ...string) []model.Job {
	var out []model.Job
	for _, r := range refs {
		out = append(out, model.Job{ReferenceNumber: r, Title: "Werkstudent " + r})
	}
	return out
}

// keys turns jobs into "source/ref" strings for easy comparison.
func keys(js []model.Job) []string {
	var out []string
	for _, j := range js {
		out = append(out, j.Source+"/"+j.ReferenceNumber)
	}
	return out
}

func assertKeys(t *testing.T, got []model.Job, want ...string) {
	t.Helper()
	g := keys(got)
	if len(g) != len(want) {
		t.Fatalf("jobs = %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("jobs = %v, want %v", g, want)
		}
	}
}

func mustRun(t *testing.T, r *Radar, now time.Time) Result {
	t.Helper()
	res, err := r.Run(context.Background(), model.Query{}, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// ---------------------------------------------------------------- happy path

func TestRunAllSourcesSucceed(t *testing.T) {
	r := &Radar{
		Storage: openStorage(t),
		Collectors: []Collector{
			&fakeCollector{name: "a", jobs: jobs("1", "2")},
			&fakeCollector{name: "b", jobs: jobs("3")},
		},
	}

	res := mustRun(t, r, t1)

	// New jobs come back with Source stamped by the radar.
	assertKeys(t, res.New, "a/1", "a/2", "b/3")

	if len(res.Sources) != 2 {
		t.Fatalf("got %d reports, want 2", len(res.Sources))
	}
	for i, want := range []SourceReport{{Name: "a", Found: 2}, {Name: "b", Found: 1}} {
		got := res.Sources[i]
		if got.Name != want.Name || got.Found != want.Found || got.Err != nil {
			t.Errorf("report %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestRunSecondRunNothingNew(t *testing.T) {
	r := &Radar{
		Storage:    openStorage(t),
		Collectors: []Collector{&fakeCollector{name: "a", jobs: jobs("1", "2")}},
	}
	mustRun(t, r, t1)

	res := mustRun(t, r, t1.Add(2*time.Hour))
	assertKeys(t, res.New)
	if res.Sources[0].Found != 2 {
		t.Errorf("Found = %d, want 2 (jobs are still found, just not new)", res.Sources[0].Found)
	}
}

func TestRunSameRefFromDifferentSources(t *testing.T) {
	r := &Radar{
		Storage: openStorage(t),
		Collectors: []Collector{
			&fakeCollector{name: "a", jobs: jobs("1")},
			&fakeCollector{name: "b", jobs: jobs("1")},
		},
	}
	res := mustRun(t, r, t1)
	assertKeys(t, res.New, "a/1", "b/1")
}

func TestRunNoCollectors(t *testing.T) {
	r := &Radar{Storage: openStorage(t)}
	res := mustRun(t, r, t1)
	if len(res.New) != 0 || len(res.Sources) != 0 {
		t.Errorf("got %+v, want an empty result", res)
	}
}

// ---------------------------------------------------------------- failing sources

func TestRunOneSourceFailsOthersStillSaved(t *testing.T) {
	boom := errors.New("api down")
	r := &Radar{
		Storage: openStorage(t),
		Collectors: []Collector{
			&fakeCollector{name: "a", jobs: jobs("1")},
			&fakeCollector{name: "broken", err: boom},
			&fakeCollector{name: "c", jobs: jobs("2")},
		},
	}

	res := mustRun(t, r, t1) // a failing source must NOT fail the run

	assertKeys(t, res.New, "a/1", "c/2")
	if !errors.Is(res.Sources[1].Err, boom) {
		t.Errorf("report for broken source: Err = %v, want %v", res.Sources[1].Err, boom)
	}
	if res.Sources[0].Err != nil || res.Sources[2].Err != nil {
		t.Errorf("healthy sources should have no error: %+v", res.Sources)
	}
}

func TestRunFailedSourceJobsAreNotSaved(t *testing.T) {
	// A source that returns jobs AND an error (e.g. pagination broke halfway):
	// the radar treats the source as failed and saves nothing from it.
	saver := &fakeSaver{}
	r := &Radar{
		Storage: saver,
		Collectors: []Collector{
			&fakeCollector{name: "ok", jobs: jobs("1")},
			&fakeCollector{name: "half", jobs: jobs("2", "3"), err: errors.New("page 2 failed")},
		},
	}
	mustRun(t, r, t1)
	assertKeys(t, saver.got, "ok/1")
}

func TestRunSlowSourceTimesOut(t *testing.T) {
	r := &Radar{
		Storage: openStorage(t),
		Timeout: 50 * time.Millisecond,
		Collectors: []Collector{
			&fakeCollector{name: "fast", jobs: jobs("1")},
			&fakeCollector{name: "stuck", block: true},
		},
	}

	start := time.Now()
	res := mustRun(t, r, t1)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Run took %v; the timeout did not stop the stuck source", took)
	}

	assertKeys(t, res.New, "fast/1")
	if !errors.Is(res.Sources[1].Err, context.DeadlineExceeded) {
		t.Errorf("stuck source: Err = %v, want context.DeadlineExceeded", res.Sources[1].Err)
	}
}

func TestRunZeroTimeoutUsesDefault(t *testing.T) {
	// Timeout 0 must not mean "already expired".
	var deadline time.Time
	var hasDeadline bool
	r := &Radar{
		Storage: openStorage(t),
		Collectors: []Collector{&fakeCollector{
			name: "a",
			jobs: jobs("1"),
			onStart: func(ctx context.Context) {
				deadline, hasDeadline = ctx.Deadline()
			},
		}},
	}

	start := time.Now()
	res := mustRun(t, r, t1)

	if res.Sources[0].Err != nil {
		t.Fatalf("source failed with Timeout 0: %v", res.Sources[0].Err)
	}
	if !hasDeadline {
		t.Fatal("collector got no deadline; every source needs a timeout")
	}
	if got := deadline.Sub(start); got < defaultTimeout-time.Second || got > defaultTimeout+time.Second {
		t.Errorf("deadline is %v away, want about %v", got, defaultTimeout)
	}
}

// ---------------------------------------------------------------- concurrency

func TestRunCollectsConcurrently(t *testing.T) {
	// Each collector waits until BOTH have started. If Run called them one
	// after another, the first would wait forever (until the timeout).
	var started sync.WaitGroup
	started.Add(2)
	bothStarted := make(chan struct{})
	go func() { started.Wait(); close(bothStarted) }()

	waitForOther := func(ctx context.Context) {
		started.Done()
		select {
		case <-bothStarted:
		case <-ctx.Done():
		}
	}

	r := &Radar{
		Storage: openStorage(t),
		Timeout: 2 * time.Second,
		Collectors: []Collector{
			&fakeCollector{name: "a", jobs: jobs("1"), onStart: waitForOther},
			&fakeCollector{name: "b", jobs: jobs("2"), onStart: waitForOther},
		},
	}

	start := time.Now()
	res := mustRun(t, r, t1)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Run took %v: collectors did not run at the same time", took)
	}
	assertKeys(t, res.New, "a/1", "b/2")
}

func TestRunReportsKeepCollectorOrder(t *testing.T) {
	// "slow" finishes last but is listed first: reports must follow the list,
	// not the order in which goroutines happen to finish.
	r := &Radar{
		Storage: openStorage(t),
		Collectors: []Collector{
			&fakeCollector{name: "slow", jobs: jobs("1"), delay: 80 * time.Millisecond},
			&fakeCollector{name: "medium", jobs: jobs("2"), delay: 40 * time.Millisecond},
			&fakeCollector{name: "fast", jobs: jobs("3")},
		},
	}
	res := mustRun(t, r, t1)

	for i, want := range []string{"slow", "medium", "fast"} {
		if res.Sources[i].Name != want {
			t.Errorf("report %d = %q, want %q", i, res.Sources[i].Name, want)
		}
	}
	assertKeys(t, res.New, "slow/1", "medium/2", "fast/3")
}

// ---------------------------------------------------------------- saving

func TestRunSaveFails(t *testing.T) {
	diskFull := errors.New("disk full")
	r := &Radar{
		Storage:    &fakeSaver{err: diskFull},
		Collectors: []Collector{&fakeCollector{name: "a", jobs: jobs("1")}},
	}

	res, err := r.Run(context.Background(), model.Query{}, t1)
	if !errors.Is(err, diskFull) {
		t.Fatalf("err = %v, want it to wrap %v", err, diskFull)
	}
	// The caller still learns what was collected.
	if len(res.Sources) != 1 || res.Sources[0].Found != 1 {
		t.Errorf("reports = %+v, want them returned even when saving fails", res.Sources)
	}
	if len(res.New) != 0 {
		t.Errorf("New = %v, want none when saving fails", keys(res.New))
	}
}

func TestRunSavesOnceWithStampedSources(t *testing.T) {
	saver := &fakeSaver{}
	r := &Radar{
		Storage: saver,
		Collectors: []Collector{
			&fakeCollector{name: "a", jobs: jobs("1", "2")},
			&fakeCollector{name: "b", jobs: jobs("3")},
		},
	}
	mustRun(t, r, t1)

	// Everything arrives in one Save call, in collector order, Source set.
	assertKeys(t, saver.got, "a/1", "a/2", "b/3")
}
