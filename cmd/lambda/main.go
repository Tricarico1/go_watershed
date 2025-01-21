package main

import (
	"context"

	"github.com/Tricarico1/go_watershed/internal/watershed"
	"github.com/aws/aws-lambda-go/lambda"
)

func handleRequest(ctx context.Context) error {
	monitor := watershed.NewMonitor()
	return monitor.RunOnce()
}

func main() {
	lambda.Start(handleRequest)
}
