// Command healthcheck is the container HEALTHCHECK probe. The runtime image is
// distroless (no shell, curl or wget), so the probe is a tiny static binary.
// It exits 0 when GET http://127.0.0.1:<port><path> answers 200.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	path := "/health/ready"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	port := "8080"
	if addr := os.Getenv("HTTP_ADDR"); addr != "" {
		port = addr[strings.LastIndex(addr, ":")+1:]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+path, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		os.Exit(1)
	}
}
