package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	InstanceID string
	LogLevel   slog.Level

	StartTimeout time.Duration
	StopTimeout  time.Duration

	HTTP     HTTP
	Database Database
	AWS      AWS
	SQS      SQS
}

type HTTP struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

type Database struct {
	// URL is secret: it contains the password. Never log it.
	URL      string
	MaxConns int32
}

// AWS configures the AWS SDK. Credentials are deliberately not part of this
// struct: the SDK reads AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY itself, which
// keeps secrets out of our own types and logs.
type AWS struct {
	Region string
	// EndpointURL overrides the AWS endpoint (LocalStack/MiniStack). Empty
	// means the real AWS endpoint for Region.
	EndpointURL string
}

type SQS struct {
	WagerQueueName  string
	DLQName         string
	EventsQueueName string
}

type LookupFunc func(key string) (string, bool)

// FromEnv loads the configuration from the process environment.
func FromEnv() (Config, error) {
	return Load(os.LookupEnv)
}

// Load reads every variable, applies defaults, validates the result and
// reports all problems at once.
func Load(lookup LookupFunc) (Config, error) {
	r := reader{lookup: lookup}

	hostname, _ := os.Hostname()
	cfg := Config{
		InstanceID:   r.str("INSTANCE_ID", hostname),
		LogLevel:     r.level("LOG_LEVEL", slog.LevelInfo),
		StartTimeout: r.duration("START_TIMEOUT", 30*time.Second),
		StopTimeout:  r.duration("SHUTDOWN_TIMEOUT", 25*time.Second),
		HTTP: HTTP{
			Addr:              r.str("HTTP_ADDR", ":8080"),
			ReadHeaderTimeout: r.duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
			ReadTimeout:       r.duration("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:      r.duration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:       r.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout:   r.duration("HTTP_SHUTDOWN_TIMEOUT", 10*time.Second),
		},
		Database: Database{
			URL:      r.required("DATABASE_URL"),
			MaxConns: r.int32("DB_MAX_CONNS", 10),
		},
		AWS: AWS{
			Region:      r.required("AWS_REGION"),
			EndpointURL: r.str("AWS_ENDPOINT_URL", ""),
		},
		SQS: SQS{
			WagerQueueName:  r.str("SQS_WAGER_QUEUE_NAME", "wager-transactions.fifo"),
			DLQName:         r.str("SQS_DLQ_NAME", "wager-transactions-dlq.fifo"),
			EventsQueueName: r.str("SQS_EVENTS_QUEUE_NAME", "wallet-events.fifo"),
		},
	}

	r.check(cfg.InstanceID != "", "INSTANCE_ID must not be empty (hostname unavailable)")
	r.check(cfg.HTTP.Addr != "", "HTTP_ADDR must not be empty")
	r.check(cfg.Database.MaxConns > 0, "DB_MAX_CONNS must be greater than zero")
	if cfg.Database.URL != "" {
		u, err := url.Parse(cfg.Database.URL)
		// Do not wrap err: url.Error includes the URL, and with it the password.
		r.check(err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql"),
			"DATABASE_URL must be a postgres:// URL")
	}
	if cfg.AWS.EndpointURL != "" {
		u, err := url.Parse(cfg.AWS.EndpointURL)
		r.check(err == nil && u.Scheme != "" && u.Host != "", "AWS_ENDPOINT_URL must be an absolute URL")
	}
	for key, name := range map[string]string{
		"SQS_WAGER_QUEUE_NAME":  cfg.SQS.WagerQueueName,
		"SQS_DLQ_NAME":          cfg.SQS.DLQName,
		"SQS_EVENTS_QUEUE_NAME": cfg.SQS.EventsQueueName,
	} {
		r.check(strings.HasSuffix(name, ".fifo"), key+" must name a FIFO queue (suffix .fifo)")
	}
	r.check(cfg.HTTP.ShutdownTimeout < cfg.StopTimeout,
		"HTTP_SHUTDOWN_TIMEOUT must be shorter than SHUTDOWN_TIMEOUT so the HTTP drain fits in the stop budget")

	if err := errors.Join(r.errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// reader accumulates parse errors so every problem is reported in one run.
type reader struct {
	lookup LookupFunc
	errs   []error
}

func (r *reader) check(ok bool, msg string) {
	if !ok {
		r.errs = append(r.errs, errors.New(msg))
	}
}

func (r *reader) str(key, def string) string {
	if v, ok := r.lookup(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (r *reader) required(key string) string {
	v := r.str(key, "")
	r.check(v != "", key+" is required")
	return v
}

func (r *reader) duration(key string, def time.Duration) time.Duration {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		r.errs = append(r.errs, fmt.Errorf("%s must be a positive duration such as 5s, got %q", key, v))
		return def
	}
	return d
}

func (r *reader) int32(key string, def int32) int32 {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be an integer, got %q", key, v))
		return def
	}
	return int32(n)
}

func (r *reader) level(key string, def slog.Level) slog.Level {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be one of debug, info, warn, error, got %q", key, v))
		return def
	}
	return l
}
