package archtest

import (
	"slices"
	"testing"
)

const module = "github.com/UmBender/backend-challenge-go"

func TestDomainImportCheckerFlagsInfrastructure(t *testing.T) {
	findings, err := DomainImportFindings("testdata/impure", module)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.Rule)
	}
	for _, banned := range []string{
		`import "database/sql"`,
		`import "log/slog"`,
		`import "net/http"`,
		`import "os"`,
		`import "github.com/jackc/pgx/v5"`,
		`import "go.uber.org/fx"`,
		`import "github.com/UmBender/backend-challenge-go/internal/adapters/postgres"`,
	} {
		if !slices.Contains(got, banned) {
			t.Errorf("%s not reported; got %v", banned, got)
		}
	}
	for _, allowed := range []string{
		`import "context"`,
		`import "github.com/google/uuid"`,
		`import "github.com/UmBender/backend-challenge-go/internal/domain/money"`,
	} {
		if slices.Contains(got, allowed) {
			t.Errorf("%s is allowed but was reported", allowed)
		}
	}
}

// §4/§6: the domain is independent of Fx, HTTP, SQS and persistence.
func TestDomainDependsOnlyOnTheStandardLibrary(t *testing.T) {
	findings, err := DomainImportFindings("../domain", module)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("%s: %s", f.Pos, f.Rule)
	}
}
