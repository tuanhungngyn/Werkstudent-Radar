package collect

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"werkstudent-radar/internal/model"
)

const (
	swDefaultBaseURL = "https://www.stellenwerk.de"
	swRequestLimit   = 1 * time.Second // default pause between job page requests
	swMaxPerRun      = 10 // job pages per run, until we can skip known jobs
)

type swSitemap struct {
	URLs []swURL `xml:"url"`
}

type swURL struct {
	Loc string `xml:"loc"`
}

// errJobGone means a job is still in the sitemap but its page was removed.
// That happens all the time and is not a reason to fail the whole source.
var errJobGone = errors.New("job page is gone")

type Stellenwerk struct {
	HTTP    *http.Client
	BaseURL string
	// Delay is the pause between two job page requests.
	// 0 means swRequestLimit; tests set it to something tiny.
	Delay time.Duration
}

type swResult struct {
	Title              string `json:"title"`
	HiringOrganization struct {
		Name string `json:"name"`
	}
	PublishDate string `json:"datePosted"`
	ExpiryDate  string `json:"validThrough"`
	JobLocation struct {
		Address struct {
			Locality string `json:"addressLocality"`
		} `json:"address"`
	} `json:"jobLocation"`
	JobDescription string `json:"description"`
}

func (s *Stellenwerk) Name() string {
	return "stellenwerk"
}

func (s *Stellenwerk) baseURL() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return swDefaultBaseURL
}

func (s *Stellenwerk) delay() time.Duration {
	if s.Delay > 0 {
		return s.Delay
	}
	return swRequestLimit
}

func (s *Stellenwerk) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// swJobPath matches job pages like /hamburg/werkstudent-it-support-261112-279990:
// city, slug, posting date (YYMMDD) and Stellenwerk's job ID.
var swJobPath = regexp.MustCompile(`^/([a-z-]+)/(.+)-(\d{6})-(\d+)$`)

// swJobRef is what a job URL tells us before fetching the page.
type swJobRef struct {
	URL  string // public URL from the sitemap, shown to the user
	Path string // path only, fetched relative to baseURL() so tests stay local
	City string
	Ref  string
}

// parseJobURL reports whether raw is a Stellenwerk job page and, if so,
// returns its city and reference number.
func parseJobURL(raw string) (swJobRef, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return swJobRef{}, false
	}
	m := swJobPath.FindStringSubmatch(u.Path)
	if m == nil || m[1] == "magazin" {
		return swJobRef{}, false
	}
	return swJobRef{URL: raw, Path: u.Path, City: m[1], Ref: m[4]}, true
}

func (s *Stellenwerk) fetchJob(ctx context.Context, u string) (swResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return swResult{}, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return swResult{}, err
	}
	// A failed Close on a response body leaves nothing to recover, so the
	// error is dropped on purpose.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return swResult{}, errJobGone
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return swResult{}, fmt.Errorf("%s: %s", resp.Status, body)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return swResult{}, err
	}
	result, err := extractPosting(string(body))
	if err != nil {
		return swResult{}, err
	}
	return result, nil
}

func (s *Stellenwerk) fetchSitemap(ctx context.Context) ([]string, error) {
	// there is only one sitemap, so no check needed
	sitemapURL := s.baseURL() + "/sitemap.xml"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sitemapURL, nil)
	if err != nil {
		return nil, err
	}
	hc := s.client()

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}

	// A failed Close on a response body leaves nothing to recover, so the
	// error is dropped on purpose.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("%s: %s", resp.Status, body)
	}

	var sw swSitemap
	if err := xml.NewDecoder(resp.Body).Decode(&sw); err != nil {
		return nil, err
	}
	var urls []string
	for _, u := range sw.URLs {
		urls = append(urls, u.Loc)
	}
	return urls, nil
}

// filterJobURLs keeps the job pages in city. city is a Stellenwerk slug
// like "hamburg" or "koeln", not the raw -location value.
func filterJobURLs(urls []string, city string) []swJobRef {
	var refs []swJobRef
	for _, raw := range urls {
		ref, ok := parseJobURL(raw)
		if ok && ref.City == city {
			refs = append(refs, ref)
		}
	}
	return refs
}

const swPostingStart = `<script type="application/ld+json">`

func extractPosting(html string) (swResult, error) {
	_, rest, found := strings.Cut(html, swPostingStart)
	if !found {
		return swResult{}, fmt.Errorf("no job posting found")
	}
	block, _, found := strings.Cut(rest, "</script>")
	if !found {
		return swResult{}, fmt.Errorf("job posting has no end")
	}

	var r swResult
	if err := json.Unmarshal([]byte(block), &r); err != nil {
		return swResult{}, fmt.Errorf("failed to parse job posting: %w", err)
	}
	return r, nil
}

func (s *Stellenwerk) fetchJobDetails(ctx context.Context, refs []swJobRef) ([]model.Job, error) {
	var results []model.Job

	for i, ref := range refs {
		// Pause between requests (not before the first) so we don't hammer the server.
		if i > 0 {
			time.Sleep(s.delay())
		}

		result, err := s.fetchJob(ctx, s.baseURL()+ref.Path)
		if errors.Is(err, errJobGone) {
			continue // removed since the sitemap was built; skip it
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref.URL, err)
		}
		results = append(results, s.toJob(result, ref))
	}

	return results, nil
}

func (s *Stellenwerk) Collect(ctx context.Context, q model.Query) ([]model.Job, error) {
	city := strings.ToLower(strings.TrimSpace(q.Location))
	if city == "" {
		return nil, errors.New("stellenwerk needs -location")
	}

	urls, err := s.fetchSitemap(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch sitemap: %w", err)
	}

	refs := filterJobURLs(urls, city)
	if len(refs) > swMaxPerRun {
		refs = refs[:swMaxPerRun]
	}

	jobs, err := s.fetchJobDetails(ctx, refs)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch job details: %w", err)
	}

	return jobs, nil
}

func (s *Stellenwerk) toJob(r swResult, ref swJobRef) model.Job {
	location := r.JobLocation.Address.Locality
	if location == "" {
		location = ref.City // the portal city from the URL
	}

	return model.Job{
		Title:           r.Title,
		Company:         r.HiringOrganization.Name,
		Location:        location,
		URL:             ref.URL,
		ReferenceNumber: ref.Ref,
		Description:     r.JobDescription,
		PublishedAt:     r.PublishDate,
		ExpiryDate:      r.ExpiryDate,
	}
}
