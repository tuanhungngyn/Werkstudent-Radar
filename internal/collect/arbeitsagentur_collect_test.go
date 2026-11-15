package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"werkstudent-radar/internal/model"
)

// fakeAA starts a local HTTP server that behaves like the Arbeitsagentur search:
// it reports `total` results and serves them in pages of PageSize.
// With alwaysFull, every page is full no matter what (a broken API), which is
// what the safety limit has to protect against.
//
// It returns the collector already pointed at the fake server, plus a counter
// of how many requests the server received.
func fakeAA(t *testing.T, total int, alwaysFull bool) (*Arbeitsagentur, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)

		if got := r.Header.Get("X-API-Key"); got != APIKey {
			t.Errorf("request %d: X-API-Key = %q, want %q", n, got, APIKey)
		}
		if !strings.HasSuffix(r.URL.Path, V6Endpoint) {
			t.Errorf("request %d: path = %q, want it to end with %q", n, r.URL.Path, V6Endpoint)
		}

		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Errorf("request %d: page parameter %q is not a number", n, r.URL.Query().Get("page"))
			http.Error(w, "bad page", http.StatusBadRequest)
			return
		}
		// Pages must be requested in order: 1, 2, 3, ...
		if page != int(n) {
			t.Errorf("request %d asked for page %d, want page %d", n, page, n)
		}

		// How many results are left for this page?
		count := total - (page-1)*PageSize
		count = max(0, min(count, PageSize))
		if alwaysFull {
			count = PageSize
		}

		resp := aaSearch{MaxResults: total}
		for i := range count {
			resp.ResultsList = append(resp.ResultsList, aaResult{
				Title: "Werkstudent Test",
				Refnr: fmt.Sprintf("ref-%d-%d", page, i),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close) // stop the server when the test ends

	return &Arbeitsagentur{BaseURL: srv.URL}, &requests
}

func TestArbeitsagenturCollectPagination(t *testing.T) {
	tests := []struct {
		name         string
		total        int
		alwaysFull   bool
		wantJobs     int
		wantRequests int
	}{
		{
			name:         "last page is short: 100 + 100 + 37",
			total:        237,
			wantJobs:     237,
			wantRequests: 3,
		},
		{
			name:         "exact multiple of the page size stops via total, no extra request",
			total:        200,
			wantJobs:     200,
			wantRequests: 2,
		},
		{
			name:         "fewer results than one page",
			total:        5,
			wantJobs:     5,
			wantRequests: 1,
		},
		{
			name:         "no results at all",
			total:        0,
			wantJobs:     0,
			wantRequests: 1,
		},
		{
			name:         "broken API with endless full pages stops at the safety limit",
			total:        1_000_000,
			alwaysFull:   true,
			wantJobs:     MaxPages * PageSize,
			wantRequests: MaxPages,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, requests := fakeAA(t, tt.total, tt.alwaysFull)

			jobs, err := a.Collect(context.Background(), model.Query{Title: "software"})
			if err != nil {
				t.Fatalf("Collect returned an error: %v", err)
			}
			if len(jobs) != tt.wantJobs {
				t.Errorf("got %d jobs, want %d", len(jobs), tt.wantJobs)
			}
			if got := int(requests.Load()); got != tt.wantRequests {
				t.Errorf("server got %d requests, want %d", got, tt.wantRequests)
			}
		})
	}
}

func TestArbeitsagenturCollectNoDuplicates(t *testing.T) {
	a, _ := fakeAA(t, 237, false)

	jobs, err := a.Collect(context.Background(), model.Query{})
	if err != nil {
		t.Fatalf("Collect returned an error: %v", err)
	}

	// If a page were fetched twice (e.g. always page 1), refnrs would repeat.
	seen := map[string]bool{}
	for _, j := range jobs {
		if seen[j.ReferenceNumber] {
			t.Fatalf("duplicate job %q: a page was fetched more than once", j.ReferenceNumber)
		}
		seen[j.ReferenceNumber] = true
	}
}

func TestArbeitsagenturCollectServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	a := &Arbeitsagentur{BaseURL: srv.URL}
	jobs, err := a.Collect(context.Background(), model.Query{})
	if err == nil {
		t.Fatal("expected an error for a 503 response, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention the status code, got: %v", err)
	}
	if jobs != nil {
		t.Errorf("expected no jobs on error, got %d", len(jobs))
	}
}
