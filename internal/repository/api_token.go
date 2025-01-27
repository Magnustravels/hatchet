package repository

import (
	"time"

	"github.com/hatchet-dev/hatchet/internal/repository/prisma/db"
)

type CreateAPITokenOpts struct {
	// The id of the token
	ID string `validate:"required,uuid"`

	// When the token expires
	ExpiresAt time.Time

	// (optional) A tenant ID for this API token
	TenantId *string `validate:"omitempty,uuid"`

	// (optional) A name for this API token
	Name *string `validate:"omitempty,max=255"`
}

type APITokenRepository interface {
	GetAPITokenById(id string) (*db.APITokenModel, error)
	CreateAPIToken(opts *CreateAPITokenOpts) (*db.APITokenModel, error)
	RevokeAPIToken(id string) error
	ListAPITokensByTenant(tenantId string) ([]db.APITokenModel, error)
}
