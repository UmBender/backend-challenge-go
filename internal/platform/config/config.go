package config

import (
	"log/slog"
	"os"
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
	// TODO
	return Config{}, nil
}
