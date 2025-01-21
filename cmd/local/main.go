package main

import (
	"flag"
	"log"
	"time"

	"github.com/Tricarico1/go_watershed/internal/watershed"
)

func main() {
	continuous := flag.Bool("continuous", false, "Run continuously every 5 minutes")
	flag.Parse()

	monitor := watershed.NewMonitor()
	log.Println("Starting monitoring service...")

	if *continuous {
		// Run in continuous mode (like the current version)
		for {
			if err := monitor.RunOnce(); err != nil {
				log.Printf("Error in monitoring cycle: %v", err)
			}
			time.Sleep(5 * time.Minute)
		}
	} else {
		// Run once and exit (better for Lambda-like testing)
		if err := monitor.RunOnce(); err != nil {
			log.Printf("Error in monitoring cycle: %v", err)
		}
	}
}
