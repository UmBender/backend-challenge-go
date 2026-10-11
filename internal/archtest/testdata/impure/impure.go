// Package impure is a fixture: a "domain" package that leaks infrastructure.
package impure

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/fx"

	"github.com/UmBender/backend-challenge-go/internal/adapters/postgres"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
)
