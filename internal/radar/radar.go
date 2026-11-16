package radar

import (
	"context"
	"fmt"
	"sync"
	"time"

	"werkstudent-radar/internal/model"
)

// defaultTimeout is used per source when Radar.Timeout is 0.
const defaultTimeout = 2 * time.Minute

type Collector interface {
	Name() string
	Collect(ctx context.Context, q model.Query) ([]model.Job, error)
}

type Saver interface {
	Save(ctx context.Context, jobs []model.Job, now time.Time) ([]model.Job, error)
}

type SourceReport struct {
	Name  string
	Found int
	Err   error
}

// sourceResult is the private slot each goroutine fills in.
type sourceResult struct {
	name string
	jobs []model.Job
	err  error
}

type Result struct {
	New     []model.Job
	Sources []SourceReport
}

type Radar struct {
	Collectors []Collector
	Storage    Saver
	Timeout    time.Duration // per source; 0 means defaultTimeout
}

// Run starts a goroutine for each source, waits for all of them, and saves
// everything they found in one go. A failing source only shows up in its
// report; Run returns an error only if saving fails.
func (r *Radar) Run(ctx context.Context, q model.Query, ts time.Time) (Result, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}

	results := make([]sourceResult, len(r.Collectors))
	var wg sync.WaitGroup

	for i, c := range r.Collectors {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			jobs, err := c.Collect(ctx, q)
			results[i] = sourceResult{
				name: c.Name(),
				jobs: jobs,
				err:  err,
			}
		})
	}
	wg.Wait()

	var reports []SourceReport
	var allJobs []model.Job

	for _, res := range results {
		reports = append(reports, SourceReport{
			Name:  res.name,
			Found: len(res.jobs),
			Err:   res.err,
		})
		if res.err != nil {
			continue
		}
		for _, j := range res.jobs {
			j.Source = res.name // j is a copy, so set it here and append the copy
			allJobs = append(allJobs, j)
		}
	}

	newJobs, err := r.Storage.Save(ctx, allJobs, ts)
	if err != nil {
		return Result{Sources: reports}, fmt.Errorf("save: %w", err)
	}
	return Result{New: newJobs, Sources: reports}, nil
}
