package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"werkstudent-radar/internal/collect"
	"werkstudent-radar/internal/model"
	"werkstudent-radar/internal/radar"
	"werkstudent-radar/internal/storage"
	"werkstudent-radar/internal/utility"
)

func main() {
	// Flags
	dbPath := flag.String("db", envOr("RADAR_DB", "radar.db"), "path to the SQLite database file")
	title := flag.String("title", "", "job title to search for")
	location := flag.String("location", "", "job location to search for")
	radius := flag.Int("radius", 0, "search radius in kilometers")
	timeout := flag.Duration("timeout", 5*time.Minute, "maximum time for a whole run")

	flag.Parse()

	// Context with deadline for whole run
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Connect/Create Storage
	st, err := storage.Open(*dbPath)
	if err != nil {
		log.Fatalf("failed to open storage: %v", err)
	}
	defer st.Close()

	// Create Radar with Collectors and Storage
	r := radar.Radar{
		Collectors: []radar.Collector{
			&collect.Arbeitsagentur{},
		},
		Storage: st,
		Timeout: 2 * time.Minute, // per source
	}
	q := model.Query{
		Title:    *title,
		Location: *location,
		Radius:   *radius,
	}

	res, err := r.Run(ctx, q, time.Now())
	if err != nil {
		log.Printf("failed to run radar: %v", err)
		utility.PrintSources(res.Sources) // still show what was collected
		os.Exit(1)
	}

	// Per-source status: failing sources are warnings, not crashes.
	utility.PrintSources(res.Sources)

	fmt.Printf("Found %d new jobs:\n", len(res.New))
	for _, j := range res.New {
		fmt.Printf("- %s | %s | %s", j.Title, j.Location, j.Company)
		if j.URL != "" {
			fmt.Printf(" | %s", j.URL)
		}
		fmt.Println()
	}
}

// envOr returns the environment variable key, or fallback if it is not set.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
