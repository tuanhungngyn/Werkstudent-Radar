package main

import (
	"context"
	"fmt"
	"log"

	"werkstudent-radar/internal/collect"
	"werkstudent-radar/internal/model"
)

func main() {
	collector := &collect.Arbeitsagentur{}
	query := model.Query{
		Location: "Hamburg",
		Radius:   50,
	}
	context := context.Background()

	jobs, err := collector.Collect(context, query)
	if err != nil {
		log.Fatalf("failed to collect jobs: %v", err)
	}

	fmt.Printf("collected %d jobs\n", len(jobs))
	for _, job := range jobs {
		fmt.Printf("%s - %s\n", job.Title, job.Location)
	}
}
