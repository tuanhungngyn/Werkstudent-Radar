package utility

import (
	"fmt"

	"werkstudent-radar/internal/radar"
)

func PrintSources(sources []radar.SourceReport) {
	for _, s := range sources {
		if s.Err != nil {
			fmt.Printf("Warning: source %s failed with error: %v\n", s.Name, s.Err)
			continue
		}
		fmt.Printf("Source %s found %d jobs\n", s.Name, s.Found)
	}
}
