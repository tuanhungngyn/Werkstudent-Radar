package collect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"werkstudent-radar/internal/model"
)

// All test data here is made up. Real Stellenwerk pages are copyrighted, so
// we only copy their *shape*: the sitemap format, the URL pattern and the
// JobPosting block.

const swSite = "https://www.stellenwerk.de"

// sitemapXML builds a sitemap like Stellenwerk's from a list of paths. The
// URLs always point at the real site, just like the real sitemap does, even
// when it is served by our fake server.
func sitemapXML(paths ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, p := range paths {
		fmt.Fprintf(&b, "<url><loc>%s%s</loc><lastmod>2026-11-01</lastmod></url>", swSite, p)
	}
	b.WriteString(`</urlset>`)
	return b.String()
}

// jobPage builds a job page with a JobPosting block, surrounded by some HTML
// so extractPosting really has to find it.
func jobPage(title, company, locality string) string {
	return `<!doctype html><html><head><title>` + title + `</title>
<script type="application/ld+json">{
  "@context": "https://schema.org/",
  "@type": "JobPosting",
  "title": "` + title + `",
  "description": "<p>Made-up description.</p>",
  "datePosted": "2026-11-01T09:00:00.000Z",
  "validThrough": "2026-12-01T09:00:00.000Z",
  "hiringOrganization": {"@type": "Organization", "name": "` + company + `"},
  "jobLocation": {"@type": "Place", "address": {"@type": "PostalAddress", "addressLocality": "` + locality + `", "addressCountry": "DE"}}
}</script>
</head><body><h1>` + title + `</h1></body></html>`
}

// fakeStellenwerk serves pages[path] for every known path (status 200),
// status[path] for paths that should fail, and 404 for everything else.
// It returns the collector pointed at the server and a function that lists
// the paths requested so far, in order.
func fakeStellenwerk(t *testing.T, pages map[string]string, status map[string]int) (*Stellenwerk, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()

		if code, ok := status[r.URL.Path]; ok {
			w.WriteHeader(code)
			return
		}
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("fake server: write %s: %v", r.URL.Path, err)
		}
	}))
	t.Cleanup(srv.Close)

	s := &Stellenwerk{BaseURL: srv.URL, Delay: time.Nanosecond}
	requested := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
	return s, requested
}

// ---------------------------------------------------------------- pure functions

func TestParseJobURL(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantOK   bool
		wantCity string
		wantRef  string
	}{
		{"job page", swSite + "/hamburg/werkstudent-it-support-mwd-261112-279990", true, "hamburg", "279990"},
		{"city with a hyphen", swSite + "/duisburg-essen/aushilfe-lager-261101-278000", true, "duisburg-essen", "278000"},
		{"slug containing digits", swSite + "/koeln/2-aushilfen-gesucht-261029-279171", true, "koeln", "279171"},
		{"other host, e.g. a test server", "http://127.0.0.1:5432/koeln/aushilfe-bar-261029-279171", true, "koeln", "279171"},
		{"query string is ignored", swSite + "/hamburg/werkstudent-261112-279990?utm_source=mail", true, "hamburg", "279990"},
		{"magazine article", swSite + "/magazin/jobs-in-der-gastronomie-gefragter-denn-je-16", false, "", ""},
		{"magazine with a job-like ending", swSite + "/magazin/some-article-261112-279990", false, "", ""},
		{"career service page", swSite + "/bochum/karriereservice", false, "", ""},
		{"city portal", swSite + "/hamburg", false, "", ""},
		{"home page", swSite, false, "", ""},
		{"date too short", swSite + "/hamburg/werkstudent-2611-279990", false, "", ""},
		{"not a URL at all", "://broken", false, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := parseJobURL(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("parseJobURL(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if ref.City != tt.wantCity || ref.Ref != tt.wantRef {
				t.Errorf("got city %q ref %q, want city %q ref %q", ref.City, ref.Ref, tt.wantCity, tt.wantRef)
			}
			if ref.URL != tt.raw {
				t.Errorf("URL = %q, want the original %q", ref.URL, tt.raw)
			}
			if !strings.HasPrefix(ref.Path, "/"+tt.wantCity+"/") {
				t.Errorf("Path = %q, want it to start with /%s/", ref.Path, tt.wantCity)
			}
		})
	}
}

func TestFilterJobURLs(t *testing.T) {
	urls := []string{
		swSite,
		swSite + "/hamburg",
		swSite + "/hamburg/werkstudent-a-261101-1",
		swSite + "/koeln/werkstudent-b-261101-2",
		swSite + "/magazin/artikel-3",
		swSite + "/hamburg/aushilfe-c-261102-4",
		swSite + "/bochum/karriereservice",
	}

	refs := filterJobURLs(urls, "hamburg")

	var got []string
	for _, r := range refs {
		got = append(got, r.Ref)
	}
	if want := "1,4"; strings.Join(got, ",") != want {
		t.Errorf("refs = %v, want %s (only Hamburg jobs, in sitemap order)", got, want)
	}

	if refs := filterJobURLs(urls, "muenchen"); len(refs) != 0 {
		t.Errorf("city without jobs: got %d refs, want 0", len(refs))
	}
}

func TestExtractPosting(t *testing.T) {
	t.Run("finds the block inside the HTML", func(t *testing.T) {
		r, err := extractPosting(jobPage("Werkstudent Test", "Test GmbH", "Hamburg"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r.Title != "Werkstudent Test" {
			t.Errorf("Title = %q", r.Title)
		}
		if r.HiringOrganization.Name != "Test GmbH" {
			t.Errorf("Company = %q", r.HiringOrganization.Name)
		}
		if r.JobLocation.Address.Locality != "Hamburg" {
			t.Errorf("Locality = %q", r.JobLocation.Address.Locality)
		}
		if r.PublishDate != "2026-11-01T09:00:00.000Z" || r.ExpiryDate != "2026-12-01T09:00:00.000Z" {
			t.Errorf("dates = %q / %q", r.PublishDate, r.ExpiryDate)
		}
		if r.JobDescription != "<p>Made-up description.</p>" {
			t.Errorf("Description = %q", r.JobDescription)
		}
	})

	t.Run("missing fields stay empty instead of failing", func(t *testing.T) {
		html := `<script type="application/ld+json">{"@type":"JobPosting","title":"Only a title"}</script>`
		r, err := extractPosting(html)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r.Title != "Only a title" || r.JobLocation.Address.Locality != "" {
			t.Errorf("got %+v", r)
		}
	})

	errorCases := []struct {
		name string
		html string
	}{
		{"no block at all", "<html><body>nothing here</body></html>"},
		{"block never ends", `<script type="application/ld+json">{"title":"x"}`},
		{"broken JSON", `<script type="application/ld+json">{"title": </script>`},
		{"empty page", ""},
	}
	for _, tt := range errorCases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := extractPosting(tt.html); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestStellenwerkToJob(t *testing.T) {
	s := &Stellenwerk{}
	ref := swJobRef{URL: swSite + "/hamburg/x-261101-42", Path: "/hamburg/x-261101-42", City: "hamburg", Ref: "42"}

	r, err := extractPosting(jobPage("Werkstudent Test", "Test GmbH", "Norderstedt"))
	if err != nil {
		t.Fatal(err)
	}
	j := s.toJob(r, ref)

	want := model.Job{
		Title:           "Werkstudent Test",
		Company:         "Test GmbH",
		Location:        "Norderstedt",
		URL:             ref.URL,
		ReferenceNumber: "42",
		Description:     "<p>Made-up description.</p>",
		PublishedAt:     "2026-11-01T09:00:00.000Z",
		ExpiryDate:      "2026-12-01T09:00:00.000Z",
	}
	if j != want {
		t.Errorf("toJob =\n  %+v\nwant\n  %+v", j, want)
	}

	t.Run("no locality falls back to the portal city", func(t *testing.T) {
		r.JobLocation.Address.Locality = ""
		if got := s.toJob(r, ref).Location; got != "hamburg" {
			t.Errorf("Location = %q, want %q", got, "hamburg")
		}
	})
}

// ---------------------------------------------------------------- Collect

func TestStellenwerkCollect(t *testing.T) {
	pages := map[string]string{
		"/sitemap.xml": sitemapXML(
			"/hamburg",                           // city portal: ignored
			"/hamburg/werkstudent-it-261101-1",   // fetched
			"/koeln/werkstudent-it-261101-2",     // other city: never fetched
			"/hamburg/werkstudent-gone-261101-3", // in the sitemap, but the page is gone (404)
			"/magazin/ein-artikel-4",             // magazine: ignored
			"/hamburg/aushilfe-cafe-261102-5",    // fetched
		),
		"/hamburg/werkstudent-it-261101-1": jobPage("Werkstudent IT", "Alpha GmbH", "Hamburg"),
		"/koeln/werkstudent-it-261101-2":   jobPage("Werkstudent Köln", "Beta AG", "Köln"),
		"/hamburg/aushilfe-cafe-261102-5":  jobPage("Aushilfe Café", "Gamma KG", ""),
	}
	s, requested := fakeStellenwerk(t, pages, nil)

	jobs, err := s.Collect(context.Background(), model.Query{Location: " Hamburg "})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2 (the removed one is skipped): %+v", len(jobs), jobs)
	}
	if jobs[0].ReferenceNumber != "1" || jobs[1].ReferenceNumber != "5" {
		t.Errorf("refs = %q, %q; want 1, 5", jobs[0].ReferenceNumber, jobs[1].ReferenceNumber)
	}
	if jobs[0].Title != "Werkstudent IT" || jobs[0].Company != "Alpha GmbH" {
		t.Errorf("first job = %+v", jobs[0])
	}
	if want := swSite + "/hamburg/werkstudent-it-261101-1"; jobs[0].URL != want {
		t.Errorf("URL = %q, want the public Stellenwerk URL %q", jobs[0].URL, want)
	}
	if jobs[1].Location != "hamburg" {
		t.Errorf("job without locality: Location = %q, want the portal city", jobs[1].Location)
	}

	wantRequests := []string{
		"/sitemap.xml",
		"/hamburg/werkstudent-it-261101-1",
		"/hamburg/werkstudent-gone-261101-3",
		"/hamburg/aushilfe-cafe-261102-5",
	}
	if got := requested(); strings.Join(got, " ") != strings.Join(wantRequests, " ") {
		t.Errorf("requests =\n  %v\nwant\n  %v", got, wantRequests)
	}
}

func TestStellenwerkCollectStopsAtLimit(t *testing.T) {
	pages := map[string]string{}
	var paths []string
	for i := range swMaxPerRun + 5 {
		p := fmt.Sprintf("/hamburg/job-%d-261101-%d", i, i)
		paths = append(paths, p)
		pages[p] = jobPage(fmt.Sprintf("Job %d", i), "Test GmbH", "Hamburg")
	}
	pages["/sitemap.xml"] = sitemapXML(paths...)
	s, requested := fakeStellenwerk(t, pages, nil)

	jobs, err := s.Collect(context.Background(), model.Query{Location: "hamburg"})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(jobs) != swMaxPerRun {
		t.Errorf("got %d jobs, want %d", len(jobs), swMaxPerRun)
	}
	if got, want := len(requested()), 1+swMaxPerRun; got != want {
		t.Errorf("made %d requests, want %d (sitemap + %d job pages)", got, want, swMaxPerRun)
	}
}

func TestStellenwerkCollectNeedsLocation(t *testing.T) {
	s, requested := fakeStellenwerk(t, map[string]string{"/sitemap.xml": sitemapXML()}, nil)

	for _, loc := range []string{"", "   "} {
		if _, err := s.Collect(context.Background(), model.Query{Location: loc}); err == nil {
			t.Errorf("Location %q: expected an error, got nil", loc)
		}
	}
	if n := len(requested()); n != 0 {
		t.Errorf("made %d requests without a location, want 0", n)
	}
}

func TestStellenwerkCollectErrors(t *testing.T) {
	const job = "/hamburg/werkstudent-261101-1"

	tests := []struct {
		name   string
		pages  map[string]string
		status map[string]int
	}{
		{
			name:   "sitemap returns 500",
			status: map[string]int{"/sitemap.xml": http.StatusInternalServerError},
		},
		{
			name:  "sitemap is not XML",
			pages: map[string]string{"/sitemap.xml": "<html>maintenance</html"},
		},
		{
			name:   "job page returns 429",
			pages:  map[string]string{"/sitemap.xml": sitemapXML(job)},
			status: map[string]int{job: http.StatusTooManyRequests},
		},
		{
			name:  "job page has no JobPosting",
			pages: map[string]string{"/sitemap.xml": sitemapXML(job), job: "<html>redesigned page</html>"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := fakeStellenwerk(t, tt.pages, tt.status)
			jobs, err := s.Collect(context.Background(), model.Query{Location: "hamburg"})
			if err == nil {
				t.Fatalf("expected an error, got %d jobs", len(jobs))
			}
			if jobs != nil {
				t.Errorf("got %d jobs together with an error, want none", len(jobs))
			}
		})
	}
}
