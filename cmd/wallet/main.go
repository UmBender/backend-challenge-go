// Command wallet runs the wallet service: HTTP API, SQS consumer and workers
// in one process. Run several replicas; correctness comes from PostgreSQL.
package main

import "github.com/UmBender/backend-challenge-go/internal/bootstrap"

func main() {
	// Run starts the app, blocks until SIGINT/SIGTERM, then stops it within
	// the stop budget. It exits non-zero if start or stop fails.
	bootstrap.App().Run()
}
