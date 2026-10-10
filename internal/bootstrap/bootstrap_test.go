package bootstrap

import (
	"testing"

	"go.uber.org/fx"
)

// TestGraphIsValid resolves the whole dependency graph without running any
// constructor: missing providers, duplicate types and cycles fail here,
// without Docker.
func TestGraphIsValid(t *testing.T) {
	if err := fx.ValidateApp(Modules(), fx.NopLogger); err != nil {
		t.Fatalf("invalid Fx graph: %v", err)
	}
}
