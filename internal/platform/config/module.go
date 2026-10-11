package config

import "go.uber.org/fx"

// Module provides the validated Config. A configuration error aborts startup.
var Module = fx.Module("config",
	fx.Provide(
		FromEnv,
		func(c Config) HTTP { return c.HTTP },
		func(c Config) Database { return c.Database },
		func(c Config) AWS { return c.AWS },
		func(c Config) SQS { return c.SQS },
		func(c Config) Pending { return c.Pending },
		func(c Config) Auth { return c.Auth },
	),
)
