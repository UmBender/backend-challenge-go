package bootstrap

import (
	"go.uber.org/fx"

	"github.com/UmBender/backend-challenge-go/internal/platform/config"
	"github.com/UmBender/backend-challenge-go/internal/platform/logging"
)

// Modules is the full application graph, without process-level options.
func Modules() fx.Option {
	return fx.Options(
		config.Module,
		logging.Module,
	)
}

// App is the production application: the graph plus logging and the start
// and stop budgets from configuration.
func App() *fx.App {
	cfg, err := config.FromEnv()
	timeouts := fx.Options()
	if err == nil {
		// On error, config.Module fails the same way inside Fx and reports it.
		timeouts = fx.Options(fx.StartTimeout(cfg.StartTimeout), fx.StopTimeout(cfg.StopTimeout))
	}
	return fx.New(Modules(), logging.FxLogger, timeouts)
}
