// Command loadtest drives the three replicas started by docker compose with
// authenticated wager traffic and reports throughput, latency percentiles,
// status codes, errors, concurrency conflicts, duplicates and outbox lag,
// then checks that every wallet still reconciles.
//
//	make test-load            # or: go run ./cmd/loadtest -duration 60s -workers 32
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type config struct {
	replicas  []string
	issuer    string
	duration  time.Duration
	workers   int
	wallets   int
	dupRatio  float64
	hotRatio  float64
	initialMn string
}

type wallet struct {
	id, player string
	processed  atomic.Int64 // bets answered 201
}

type sent struct {
	w    *wallet
	ext  string
	body []byte
}

type sample struct {
	latency time.Duration
	code    int // 0 = transport error
	replay  bool
}

func main() {
	cfg := config{}
	var replicas string
	flag.StringVar(&replicas, "replicas", "http://127.0.0.1:8081,http://127.0.0.1:8082,http://127.0.0.1:8083", "replica base URLs")
	flag.StringVar(&cfg.issuer, "issuer", env("OIDC_ISSUER", "http://localhost:8180/realms/wagering"), "Keycloak realm URL")
	flag.DurationVar(&cfg.duration, "duration", 60*time.Second, "load duration")
	flag.IntVar(&cfg.workers, "workers", 32, "concurrent clients")
	flag.IntVar(&cfg.wallets, "wallets", 50, "wallets receiving bets")
	flag.Float64Var(&cfg.dupRatio, "dup-ratio", 0.10, "fraction of requests that resend a previous operation")
	flag.Float64Var(&cfg.hotRatio, "hot-ratio", 0.20, "fraction of new bets sent to one hot wallet (lock contention)")
	flag.StringVar(&cfg.initialMn, "initial-balance", "1000000.00", "initial balance of every wallet")
	flag.Parse()
	cfg.replicas = strings.Split(replicas, ",")
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var client = &http.Client{
	Timeout:   15 * time.Second,
	Transport: &http.Transport{MaxIdleConnsPerHost: 256, MaxConnsPerHost: 256},
}

func run(cfg config) error {
	provider, err := token(cfg.issuer, "provider-a")
	if err != nil {
		return err
	}
	internal, err := token(cfg.issuer, "wallet-internal")
	if err != nil {
		return err
	}
	wallets := make([]*wallet, cfg.wallets+1) // the last one is the hot wallet
	for i := range wallets {
		if wallets[i], err = openWallet(cfg, internal); err != nil {
			return err
		}
	}
	hot := wallets[len(wallets)-1]
	before := scrape(cfg.replicas)

	var (
		mu      sync.Mutex
		samples = make([]sample, 0, 1<<16)
		history []sent
		maxLag  float64
	)
	stopLag := make(chan struct{})
	go func() { // outbox lag sampled every second during the run
		for {
			select {
			case <-stopLag:
				return
			case <-time.After(time.Second):
				if lag := scrape(cfg.replicas)["outbox_lag_seconds"]; lag > maxLag {
					mu.Lock()
					maxLag = lag
					mu.Unlock()
				}
			}
		}
	}()

	start := time.Now()
	deadline := start.Add(cfg.duration)
	var wg sync.WaitGroup
	var seq atomic.Int64
	for wkr := range cfg.workers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(wkr), uint64(time.Now().UnixNano())))
			for i := 0; time.Now().Before(deadline); i++ {
				base := cfg.replicas[(wkr+i)%len(cfg.replicas)]
				var op sent
				mu.Lock()
				dup := len(history) > 0 && rng.Float64() < cfg.dupRatio
				if dup {
					op = history[rng.IntN(len(history))]
				}
				mu.Unlock()
				if !dup {
					w := wallets[rng.IntN(cfg.wallets)]
					if rng.Float64() < cfg.hotRatio {
						w = hot
					}
					ext := "load-" + strconv.FormatInt(seq.Add(1), 10) + "-" + uuid.NewString()[24:]
					op = sent{w: w, ext: ext, body: betBody(w, ext)}
				}
				s := submit(base, provider, op)
				mu.Lock()
				samples = append(samples, s)
				if s.code == 201 {
					op.w.processed.Add(1)
					history = append(history, op)
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(stopLag)

	drainStart := time.Now()
	drained := false
	for time.Since(drainStart) < 2*time.Minute {
		if scrape(cfg.replicas)["outbox_lag_seconds"] == 0 {
			drained = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	drainTime := time.Since(drainStart)
	after := scrape(cfg.replicas)

	consistent, balanceOK := 0, 0
	for _, w := range wallets {
		if ok, bal := reconcile(cfg, internal, w); ok {
			consistent++
			if bal == expectedBalance(cfg.initialMn, w.processed.Load()) {
				balanceOK++
			}
		}
	}

	report(cfg, samples, elapsed, before, after, maxLag, drained, drainTime, len(wallets), consistent, balanceOK)
	if consistent != len(wallets) || balanceOK != len(wallets) {
		return fmt.Errorf("financial check failed: %d/%d consistent, %d/%d expected balances", consistent, len(wallets), balanceOK, len(wallets))
	}
	return nil
}

func token(issuer, clientID string) (string, error) {
	resp, err := http.PostForm(issuer+"/protocol/openid-connect/token", url.Values{
		"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientID + "-secret"},
	})
	if err != nil {
		return "", fmt.Errorf("token (is the stack up?): %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.AccessToken == "" {
		return "", fmt.Errorf("token for %s: status %d", clientID, resp.StatusCode)
	}
	return body.AccessToken, nil
}

func openWallet(cfg config, tok string) (*wallet, error) {
	player := uuid.NewString()
	body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`, player, cfg.initialMn)
	req, _ := http.NewRequest(http.MethodPost, cfg.replicas[0]+"/wallets", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct{ ID string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 201 {
		return nil, fmt.Errorf("open wallet: status %d", resp.StatusCode)
	}
	return &wallet{id: out.ID, player: player}, nil
}

func betBody(w *wallet, ext string) []byte {
	return fmt.Appendf(nil, `{"providerId":"provider-a","externalTransactionId":%q,"playerId":%q,"walletId":%q,`+
		`"roundId":"load","gameId":"load","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}`, ext, w.player, w.id)
}

func submit(base, tok string, op sent) sample {
	req, _ := http.NewRequest(http.MethodPost, base+"/wagering/transactions", bytes.NewReader(op.body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "provider-a:"+op.ext)
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return sample{latency: time.Since(t0)}
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return sample{latency: time.Since(t0), code: resp.StatusCode, replay: bytes.Contains(raw, []byte(`"idempotentReplay":true`))}
}

func reconcile(cfg config, tok string, w *wallet) (bool, string) {
	req, _ := http.NewRequest(http.MethodPost, cfg.replicas[1]+"/wallets/"+w.id+"/reconciliation", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := client.Do(req)
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	var out struct {
		Consistent    bool `json:"consistent"`
		StoredBalance struct {
			Amount string `json:"amount"`
		} `json:"storedBalance"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Consistent, out.StoredBalance.Amount
}

// expectedBalance subtracts n bets of 1.00 from the initial balance, in
// integer centavos.
func expectedBalance(initial string, n int64) string {
	whole, frac, _ := strings.Cut(initial, ".")
	cents, _ := strconv.ParseInt(whole+frac, 10, 64)
	cents -= n * 100
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// scrape sums the given counters and takes the max of the lag gauge across replicas.
func scrape(replicas []string) map[string]float64 {
	out := map[string]float64{}
	for _, base := range replicas {
		resp, err := client.Get(base + "/metrics")
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		for _, line := range strings.Split(string(raw), "\n") {
			name, value, ok := strings.Cut(line, " ")
			if !ok || strings.HasPrefix(name, "#") {
				continue
			}
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				continue
			}
			switch {
			case name == "outbox_lag_seconds":
				out[name] = max(out[name], v)
			case name == "wager_concurrency_conflicts_total",
				name == `wager_duplicates_total{origin="http"}`,
				name == "outbox_events_published_total":
				out[name] += v
			}
		}
	}
	return out
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(p*float64(len(sorted))))]
}

func report(cfg config, samples []sample, elapsed time.Duration, before, after map[string]float64,
	maxLag float64, drained bool, drainTime time.Duration, wallets, consistent, balanceOK int) {
	lat := make([]time.Duration, 0, len(samples))
	codes := map[int]int{}
	replays := 0
	for _, s := range samples {
		codes[s.code]++
		if s.code != 0 {
			lat = append(lat, s.latency)
		}
		if s.replay {
			replays++
		}
	}
	slices.Sort(lat)
	ms := func(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }
	keys := make([]int, 0, len(codes))
	for c := range codes {
		keys = append(keys, c)
	}
	slices.Sort(keys)
	var byCode []string
	for _, c := range keys {
		label := strconv.Itoa(c)
		if c == 0 {
			label = "transport error"
		}
		byCode = append(byCode, fmt.Sprintf("%s: %d", label, codes[c]))
	}

	fmt.Printf("## Load test result\n\n")
	fmt.Printf("| Item | Value |\n| --- | --- |\n")
	fmt.Printf("| Environment | %d replicas (docker compose), client GOMAXPROCS=%d on %s/%s |\n", len(cfg.replicas), runtime.GOMAXPROCS(0), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("| Load | %d workers for %s, %d wallets + 1 hot wallet (%.0f%% of new bets), %.0f%% resends |\n",
		cfg.workers, cfg.duration, cfg.wallets, cfg.hotRatio*100, cfg.dupRatio*100)
	fmt.Printf("| Requests | %d in %s → **%.1f req/s** |\n", len(samples), elapsed.Round(time.Millisecond), float64(len(samples))/elapsed.Seconds())
	fmt.Printf("| New bets processed | %d (%.1f/s) |\n", codes[201], float64(codes[201])/elapsed.Seconds())
	fmt.Printf("| Latency | p50 %s · p95 %s · p99 %s · max %s |\n", ms(percentile(lat, .50)), ms(percentile(lat, .95)), ms(percentile(lat, .99)), ms(percentile(lat, 1)))
	fmt.Printf("| Status codes | %s |\n", strings.Join(byCode, " · "))
	fmt.Printf("| Errors (non 2xx or transport) | %d |\n", len(samples)-codes[200]-codes[201])
	fmt.Printf("| Duplicates answered as replay | %d (server metric: %.0f) |\n", replays,
		after[`wager_duplicates_total{origin="http"}`]-before[`wager_duplicates_total{origin="http"}`])
	fmt.Printf("| Concurrency conflicts retried | %.0f |\n", after["wager_concurrency_conflicts_total"]-before["wager_concurrency_conflicts_total"])
	fmt.Printf("| Outbox | %.0f events published · max lag during load %.2f s · drained %v after %s |\n",
		after["outbox_events_published_total"]-before["outbox_events_published_total"], maxLag, drained, drainTime.Round(time.Millisecond))
	fmt.Printf("| Reconciliation | %d/%d wallets consistent · %d/%d with the expected balance |\n", consistent, wallets, balanceOK, wallets)
}
