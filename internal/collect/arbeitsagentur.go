package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"werkstudent-radar/internal/model"
)

const (
	BaseURL      = "https://rest.arbeitsagentur.de/jobboerse/jobsuche-service"
	APIKey       = "jobboerse-jobsuche"
	PageSize     = 100
	V6Endpoint   = "/pc/v6/jobs"
	aaSearchTerm = "Werkstudent"
)

type aaSearch struct {
	MaxResults  int        `json:"maxErgebnisse"`
	ResultsList []aaResult `json:"ergebnisliste"`
}

type aaResult struct {
	Title       string `json:"stellenangebotsTitel"`
	Company     string `json:"firma"`
	Kind        string `json:"stellenangebotsart"`
	PublishDate string `json:"datumErsteVeroeffentlichung"`
	Homeoffice  bool   `json:"homeofficemoeglich"`
	ExternalURL string `json:"externeURL"`
	Refnr       string `json:"referenznummer"`
	Location    []struct {
		Adresse struct {
			PLZ string `json:"plz"`
			Ort string `json:"ort"`
		} `json:"adresse"`
		Lat float64 `json:"breite"`
		Lon float64 `json:"laenge"`
	} `json:"stellenlokationen"`
}

type aaDetail struct {
	Description string `json:"stellenangebotsBeschreibung"`
}

type Arbeitsagentur struct {
	HTTP *http.Client
}

func (a *Arbeitsagentur) Name() string {
	return "arbeitsagentur"
}

func (a *Arbeitsagentur) Collect(ctx context.Context, q model.Query) ([]model.Job, error) {
	var sr aaSearch
	if err := a.get(ctx, a.searchURL(q, 1), &sr); err != nil {
		return nil, err
	}
	jobs := make([]model.Job, 0, len(sr.ResultsList))
	for _, r := range sr.ResultsList {
		jobs = append(jobs, a.toJob(r))
	}
	return jobs, nil
}

func (a *Arbeitsagentur) get(ctx context.Context, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", APIKey)
	req.Header.Set("Accept", "application/json")

	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}

	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%s: %s (search is v6, details are v4)", resp.Status, body)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func (a *Arbeitsagentur) searchURL(q model.Query, page int) string {
	keys := url.Values{}
	keys.Set("page", strconv.Itoa(page))
	keys.Set("size", strconv.Itoa(PageSize))

	joinedTitle := aaSearchTerm
	if q.Title != "" {
		joinedTitle = joinedTitle + " " + q.Title
	}

	keys.Set("was", joinedTitle)

	if q.Location != "" {
		keys.Set("wo", q.Location)
	}
	if q.Radius != 0 {
		keys.Set("umkreis", strconv.Itoa(q.Radius))
	}

	return BaseURL + V6Endpoint + "?" + keys.Encode()
}

func (a *Arbeitsagentur) toJob(r aaResult) model.Job {
	location := ""
	if len(r.Location) > 0 {
		location = r.Location[0].Adresse.PLZ + " " + r.Location[0].Adresse.Ort
	}

	return model.Job{
		Company:         r.Company,
		Title:           r.Title,
		Location:        location,
		Remote:          r.Homeoffice,
		URL:             r.ExternalURL,
		PublishedAt:     r.PublishDate,
		ReferenceNumber: r.Refnr,
		Description:     "", // Details are not available in the search results, would require an additional request to fetch
	}
}
