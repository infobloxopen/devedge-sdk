// Hand-written ent wiring for the Fleet/Vehicle relationship fixture. Wires the
// generated ent client into the persistence.Repository seam that the generated
// batch wrappers (fleet.batch.ent.go / vehicle.batch.ent.go) embed. Mirrors
// testdata/apikey/apikeyv1/ent_wiring.go, minus secret fields and soft-delete
// (Fleet/Vehicle have neither). Provides New<R>EntRepository + fromEnt<R>.
package fleetv1

import (
	"context"
	"fmt"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/entrepo"
	"github.com/infobloxopen/devedge-sdk/testdata/fleet/ent"
	entfleet "github.com/infobloxopen/devedge-sdk/testdata/fleet/ent/fleet"
	entvehicle "github.com/infobloxopen/devedge-sdk/testdata/fleet/ent/vehicle"
)

// NewFleetEntRepository wires the ent client into a persistence.Repository for Fleet.
func NewFleetEntRepository(client *ent.Client) persistence.Repository[*Fleet, string] {
	return &entrepo.EntRepository[*Fleet, string]{
		Create_: func(ctx context.Context, entity *Fleet) (*Fleet, error) {
			if tenantID := middleware.TenantIDFromContext(ctx); entity.AccountId == "" && tenantID != "" {
				entity.AccountId = tenantID
			}
			created, err := client.Fleet.Create().
				SetID(entity.Id).
				SetAccountID(entity.AccountId).
				SetDisplayName(entity.DisplayName).
				Save(ctx)
			if err != nil {
				// Classify driver errors so a unique/FK/not-null violation becomes a
				// clean ErrConflict/ErrPreconditionFailed (409/412) with no raw SQL.
				if ce := persistence.ConstraintError(err); ce != nil {
					return nil, ce
				}
				return nil, fmt.Errorf("create fleet: %w", err)
			}
			return fromEntFleet(created), nil
		},
		Get_: func(ctx context.Context, key string) (*Fleet, error) {
			e, err := client.Fleet.Get(ctx, key) // TenantMixin interceptor scopes the query
			if err != nil {
				if ent.IsNotFound(err) {
					return nil, persistence.ErrNotFound
				}
				return nil, err
			}
			return fromEntFleet(e), nil
		},
		List_: func(ctx context.Context, opts persistence.ListOptions) ([]*Fleet, string, error) {
			if opts.PageSize <= 0 {
				opts.PageSize = 50
			}
			offset := 0
			if opts.PageToken != "" {
				fmt.Sscanf(opts.PageToken, "%d", &offset) //nolint:errcheck
			}
			items, err := client.Fleet.Query().Limit(opts.PageSize).Offset(offset).All(ctx)
			if err != nil {
				return nil, "", err
			}
			out := make([]*Fleet, len(items))
			for i, e := range items {
				out[i] = fromEntFleet(e)
			}
			next := ""
			if len(items) == opts.PageSize {
				next = fmt.Sprintf("%d", offset+opts.PageSize)
			}
			return out, next, nil
		},
		Update_: func(ctx context.Context, key string, entity *Fleet, _ ...string) (*Fleet, error) {
			u := client.Fleet.UpdateOneID(key).SetDisplayName(entity.DisplayName)
			// ent query interceptors do NOT run for mutations: scope by tenant explicitly.
			if tenantID := middleware.TenantIDFromContext(ctx); tenantID != "" {
				u = u.Where(entfleet.AccountID(tenantID))
			}
			updated, err := u.Save(ctx)
			if err != nil {
				if ent.IsNotFound(err) {
					return nil, persistence.ErrNotFound
				}
				return nil, err
			}
			return fromEntFleet(updated), nil
		},
		Delete_: func(ctx context.Context, key string) error {
			d := client.Fleet.Delete().Where(entfleet.ID(key))
			if tenantID := middleware.TenantIDFromContext(ctx); tenantID != "" {
				d = d.Where(entfleet.AccountID(tenantID))
			}
			n, err := d.Exec(ctx)
			if err != nil {
				return err
			}
			if n == 0 {
				return persistence.ErrNotFound
			}
			return nil
		},
	}
}

// fromEntFleet converts a generated ent.Fleet to the proto *Fleet.
func fromEntFleet(e *ent.Fleet) *Fleet {
	if e == nil {
		return nil
	}
	return &Fleet{
		Id:          e.ID,
		AccountId:   e.AccountID,
		DisplayName: e.DisplayName,
	}
}

// NewVehicleEntRepository wires the ent client into a persistence.Repository for Vehicle.
func NewVehicleEntRepository(client *ent.Client) persistence.Repository[*Vehicle, string] {
	return &entrepo.EntRepository[*Vehicle, string]{
		Create_: func(ctx context.Context, entity *Vehicle) (*Vehicle, error) {
			if tenantID := middleware.TenantIDFromContext(ctx); entity.AccountId == "" && tenantID != "" {
				entity.AccountId = tenantID
			}
			b := client.Vehicle.Create().
				SetID(entity.Id).
				SetAccountID(entity.AccountId).
				SetVin(entity.Vin)
			// SetFleetID sets both the scalar fleet_id column and the belongs_to edge
			// (ent unifies them because the edge declares .Field("fleet_id")).
			if entity.FleetId != "" {
				b = b.SetFleetID(entity.FleetId)
			}
			created, err := b.Save(ctx)
			if err != nil {
				if ce := persistence.ConstraintError(err); ce != nil {
					return nil, ce
				}
				return nil, fmt.Errorf("create vehicle: %w", err)
			}
			return fromEntVehicle(created), nil
		},
		Get_: func(ctx context.Context, key string) (*Vehicle, error) {
			e, err := client.Vehicle.Get(ctx, key)
			if err != nil {
				if ent.IsNotFound(err) {
					return nil, persistence.ErrNotFound
				}
				return nil, err
			}
			return fromEntVehicle(e), nil
		},
		List_: func(ctx context.Context, opts persistence.ListOptions) ([]*Vehicle, string, error) {
			if opts.PageSize <= 0 {
				opts.PageSize = 50
			}
			offset := 0
			if opts.PageToken != "" {
				fmt.Sscanf(opts.PageToken, "%d", &offset) //nolint:errcheck
			}
			items, err := client.Vehicle.Query().Limit(opts.PageSize).Offset(offset).All(ctx)
			if err != nil {
				return nil, "", err
			}
			out := make([]*Vehicle, len(items))
			for i, e := range items {
				out[i] = fromEntVehicle(e)
			}
			next := ""
			if len(items) == opts.PageSize {
				next = fmt.Sprintf("%d", offset+opts.PageSize)
			}
			return out, next, nil
		},
		Update_: func(ctx context.Context, key string, entity *Vehicle, _ ...string) (*Vehicle, error) {
			u := client.Vehicle.UpdateOneID(key).SetVin(entity.Vin)
			if entity.FleetId != "" {
				u = u.SetFleetID(entity.FleetId) // reparent via the FK-backed edge
			}
			if tenantID := middleware.TenantIDFromContext(ctx); tenantID != "" {
				u = u.Where(entvehicle.AccountID(tenantID))
			}
			updated, err := u.Save(ctx)
			if err != nil {
				if ent.IsNotFound(err) {
					return nil, persistence.ErrNotFound
				}
				return nil, err
			}
			return fromEntVehicle(updated), nil
		},
		Delete_: func(ctx context.Context, key string) error {
			d := client.Vehicle.Delete().Where(entvehicle.ID(key))
			if tenantID := middleware.TenantIDFromContext(ctx); tenantID != "" {
				d = d.Where(entvehicle.AccountID(tenantID))
			}
			n, err := d.Exec(ctx)
			if err != nil {
				return err
			}
			if n == 0 {
				return persistence.ErrNotFound
			}
			return nil
		},
	}
}

// fromEntVehicle converts a generated ent.Vehicle to the proto *Vehicle.
// FleetID is the FK-backed edge field exposed as a first-class scalar.
func fromEntVehicle(e *ent.Vehicle) *Vehicle {
	if e == nil {
		return nil
	}
	return &Vehicle{
		Id:        e.ID,
		AccountId: e.AccountID,
		Vin:       e.Vin,
		FleetId:   e.FleetID,
	}
}
