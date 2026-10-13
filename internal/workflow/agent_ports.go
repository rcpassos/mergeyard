package workflow

import (
	"context"
	"database/sql"
	"errors"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
)

// ReconcileAgentPorts fills missing leases for legacy or revived runs before
// dispatch. Existing leases retain their original geometry after config edits.
// Exhausted runs remain unassigned so lifecycle observation can release leases;
// EnsureAgentPorts prevents an unassigned run from launching an agent.
func (w *Workflow) ReconcileAgentPorts(ctx context.Context, ports config.AgentPorts) error {
	if err := ports.Validate(); err != nil {
		return err
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM runs WHERE state NOT IN ('FAILED','STOPPED','COMPLETED')
 AND id NOT IN (SELECT run_id FROM agent_port_leases) ORDER BY created_at,id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := allocateAgentPorts(ctx, tx, id, ports); err != nil {
			var coded *fault.Error
			if errors.As(err, &coded) && coded.Code == "agent_ports.exhausted" {
				continue
			}
			return err
		}
	}
	return tx.Commit()
}

// EnsureAgentPorts also covers direct handback/retry launches between ticks.
func (w *Workflow) EnsureAgentPorts(ctx context.Context, id string, ports config.AgentPorts) (config.AgentPorts, error) {
	if err := ports.Validate(); err != nil {
		return config.AgentPorts{}, err
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return config.AgentPorts{}, err
	}
	defer tx.Rollback()
	var state State
	if err := tx.QueryRowContext(ctx, "SELECT state FROM runs WHERE id=?", id).Scan(&state); err != nil {
		return config.AgentPorts{}, err
	}
	if state.Terminal() {
		return config.AgentPorts{}, invalid("Terminal runs cannot reserve agent ports")
	}
	lease, err := allocateAgentPorts(ctx, tx, id, ports)
	if err != nil {
		return config.AgentPorts{}, err
	}
	return lease, tx.Commit()
}

func loadAgentPorts(ctx context.Context, db queryer, id string) (*config.AgentPorts, error) {
	var ports config.AgentPorts
	err := db.QueryRowContext(ctx, "SELECT base,size FROM agent_port_leases WHERE run_id=?", id).Scan(&ports.Base, &ports.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &ports, err
}

func allocateAgentPorts(ctx context.Context, tx *sql.Tx, id string, ports config.AgentPorts) (config.AgentPorts, error) {
	if saved, err := loadAgentPorts(ctx, tx, id); err != nil {
		return config.AgentPorts{}, err
	} else if saved != nil {
		return *saved, nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT base,size FROM agent_port_leases ORDER BY base")
	if err != nil {
		return config.AgentPorts{}, err
	}
	base := ports.Base
	for rows.Next() {
		var occupied config.AgentPorts
		if err := rows.Scan(&occupied.Base, &occupied.Size); err != nil {
			rows.Close()
			return config.AgentPorts{}, err
		}
		if base+ports.Size <= occupied.Base {
			break
		}
		if base < occupied.Base+occupied.Size {
			// Keep candidates on the configured slot grid, even when an old
			// lease uses a different base or size.
			end := occupied.Base + occupied.Size
			base = ports.Base + ((end-ports.Base+ports.Size-1)/ports.Size)*ports.Size
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return config.AgentPorts{}, err
	}
	if base > 65536-ports.Size {
		return config.AgentPorts{}, &fault.Error{Code: "agent_ports.exhausted", Message: "No agent port block is available; finish or stop retained runs, or configure another range"}
	}
	ports.Base = base
	_, err = tx.ExecContext(ctx, "INSERT INTO agent_port_leases(run_id,base,size) VALUES(?,?,?)", id, ports.Base, ports.Size)
	return ports, err
}
