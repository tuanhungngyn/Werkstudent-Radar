package collect

import (
	"encoding/json"
	"os"
	"testing"
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
