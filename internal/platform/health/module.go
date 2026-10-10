package health

import "go.uber.org/fx"

// Module provides *Readiness, built from every Check in the readiness group.
var Module = fx.Module("health", fx.Provide(NewReadiness))
