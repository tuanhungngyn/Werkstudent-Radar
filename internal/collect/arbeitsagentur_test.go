package collect

import (
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"werkstudent-radar/internal/model"
)

func TestArbeitsagenturMapping(t *testing.T) {
	data, err := os.ReadFile("testdata/arbeitsagentur_search.json")
	if err != nil {
		t.Fatal(err)
	}

	var sr aaSearch
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sr.MaxResults == 0 {
		t.Error("maxErgebnisse not decoded")
	}
	if len(sr.ResultsList) == 0 {
		t.Fatal("no results in fixture")
	}

	a := &Arbeitsagentur{}
	j := a.toJob(sr.ResultsList[0])

	if j.Title == "" {
		t.Error("title not decoded")
	}
	if j.Company == "" {
		t.Error("company not decoded")
	}
	if j.Location == "" {
		t.Error("location not decoded")
	}
	if j.URL == "" {
		t.Error("url not decoded")
	}
	if j.PublishedAt == "" {
		t.Error("published date not decoded")
	}
	if j.ReferenceNumber == "" {
		t.Error("reference number not decoded")
	}
}

// TestArbeitsagenturQueryMapping checks that searchURL translates a model.Query
// into the API's parameters. It is "table-driven": each entry in tests is one
// case, and the loop below runs all of them as subtests.
func TestArbeitsagenturQueryMapping(t *testing.T) {
	tests := []struct {
		name string
		q    model.Query
		page int
		// want: parameter -> expected value. "" means "must NOT be in the URL".
		want map[string]string
	}{
		{
			name: "full query",
			q:    model.Query{Title: "software", Location: "München", Radius: 20},
			page: 2,
			want: map[string]string{
				"was":     "Werkstudent software",
				"wo":      "München",
				"umkreis": "20",
				"page":    "2",
				"size":    strconv.Itoa(PageSize),
			},
		},
		{
			name: "empty title still searches for werkstudent",
			q:    model.Query{Location: "Berlin"},
			page: 1,
			want: map[string]string{
				"was": "Werkstudent",
				"wo":  "Berlin",
			},
		},
		{
			name: "no location and no radius leaves them out",
			q:    model.Query{Title: "data"},
			page: 1,
			want: map[string]string{
				"was":     "Werkstudent data",
				"wo":      "",
				"umkreis": "",
			},
		},
	}

	a := &Arbeitsagentur{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := a.searchURL(tt.q, tt.page)

			// Parse the URL back instead of comparing strings:
			// parameter order and escaping don't matter this way.
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("searchURL returned an invalid URL %q: %v", raw, err)
			}
			if !strings.HasSuffix(u.Path, V6Endpoint) {
				t.Errorf("path = %q, want it to end with %q", u.Path, V6Endpoint)
			}

			params := u.Query()
			for key, want := range tt.want {
				if want == "" {
					if params.Has(key) {
						t.Errorf("%s should not be set, got %q", key, params.Get(key))
					}
					continue
				}
				if got := params.Get(key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

// TestArbeitsagenturToJobEdgeCases checks that toJob copes with incomplete
// results instead of crashing.
func TestArbeitsagenturToJobEdgeCases(t *testing.T) {
	a := &Arbeitsagentur{}

	t.Run("no location does not panic", func(t *testing.T) {
		j := a.toJob(aaResult{Title: "Werkstudent Backend"})
		if j.Location != "" {
			t.Errorf("Location = %q, want empty", j.Location)
		}
		if j.Title != "Werkstudent Backend" {
			t.Errorf("Title = %q, want %q", j.Title, "Werkstudent Backend")
		}
	})

	// Location is a slice of an anonymous struct, which is awkward to write as a
	// Go literal, so these cases build the aaResult from a small JSON snippet.
	locationCases := []struct {
		name string
		json string
		want string
	}{
		{
			name: "one location is formatted as PLZ + city",
			json: `{"stellenlokationen": [{"adresse": {"plz": "80333", "ort": "München"}}]}`,
			want: "80333 München",
		},
		{
			name: "several locations: the first one wins",
			json: `{"stellenlokationen": [
				{"adresse": {"plz": "10115", "ort": "Berlin"}},
				{"adresse": {"plz": "20095", "ort": "Hamburg"}}
			]}`,
			want: "10115 Berlin",
		},
		{
			name: "empty location list",
			json: `{"stellenlokationen": []}`,
			want: "",
		},
	}
	for _, tt := range locationCases {
		t.Run(tt.name, func(t *testing.T) {
			var r aaResult
			if err := json.Unmarshal([]byte(tt.json), &r); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := a.toJob(r).Location; got != tt.want {
				t.Errorf("Location = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("every fixture result maps without panicking", func(t *testing.T) {
		data, err := os.ReadFile("testdata/arbeitsagentur_search.json")
		if err != nil {
			t.Fatal(err)
		}
		var sr aaSearch
		if err := json.Unmarshal(data, &sr); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for i, r := range sr.ResultsList {
			j := a.toJob(r)
			if j.ReferenceNumber == "" {
				t.Errorf("result %d: ReferenceNumber is empty", i)
			}
		}
	})
}
