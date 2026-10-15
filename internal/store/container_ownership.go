//go:build darwin || linux

package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/faustbrian/reviewd/internal/containerapi"
)

// PrivateContainer extends the existing private owner; it is not a second
// resource journal or lease. The reservation precedes every daemon create.
type PrivateContainer struct {
	owner *PrivateOwner
	id    string
}

func (o *PrivateOwner) StartingContainer(ctx context.Context, endpoint, daemonID string) (*PrivateContainer, error) {
	if _, err := o.lease.Info(); err != nil {
		return nil, errors.New("private container owner lease is closed")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	reference := containerapi.Resource{Endpoint: endpoint, DaemonID: daemonID, OwnerID: o.ID, ResourceID: id}
	if !reference.Valid() {
		return nil, containerapi.ErrUncertain
	}
	raw, err := json.Marshal(ownedContainer{Kind: "docker", Resource: reference})
	if err != nil {
		return nil, err
	}
	result, err := o.store.db.ExecContext(ctx, `UPDATE private_owners SET processes=json_set(processes,?,json(?)) WHERE id=? AND closing=0 AND (SELECT count(*) FROM json_each(processes))<16`, processPath(id), string(raw), o.ID)
	if err := changed(result, err, "cannot reserve private container creation"); err != nil {
		return nil, err
	}
	return &PrivateContainer{owner: o, id: id}, nil
}
func decodeContainer(raw []byte, ownerID, id string) (containerapi.Resource, error) {
	var value ownedContainer
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || value.Kind != "docker" || !value.Resource.Valid() || value.Resource.OwnerID != ownerID || value.Resource.ResourceID != id {
		return containerapi.Resource{}, containerapi.ErrUncertain
	}
	return value.Resource, nil
}
func (p *PrivateContainer) load(ctx context.Context) (containerapi.Resource, string, error) {
	var raw sql.NullString
	if err := p.owner.store.db.QueryRowContext(ctx, `SELECT json_extract(processes,?) FROM private_owners WHERE id=?`, processPath(p.id), p.owner.ID).Scan(&raw); err != nil {
		return containerapi.Resource{}, "", err
	}
	if !raw.Valid {
		return containerapi.Resource{}, "", sql.ErrNoRows
	}
	reference, err := decodeContainer([]byte(raw.String), p.owner.ID, p.id)
	return reference, raw.String, err
}
func (p *PrivateContainer) Resource(ctx context.Context) (containerapi.Resource, error) {
	reference, _, err := p.load(ctx)
	return reference, err
}
func (p *PrivateContainer) Bind(ctx context.Context, id string) error {
	reference, raw, err := p.load(ctx)
	if err != nil {
		return errors.Join(containerapi.ErrUncertain, err)
	}
	if reference.ContainerID != "" && reference.ContainerID != id {
		return containerapi.ErrUncertain
	}
	reference.ContainerID = id
	if id == "" || !reference.Valid() {
		return containerapi.ErrUncertain
	}
	client, err := containerapi.New(reference.Endpoint)
	if err != nil {
		return err
	}
	defer client.Close()
	observed, err := client.Observe(ctx, reference)
	if err != nil || !observed.Present || observed.ContainerID != id {
		return errors.Join(containerapi.ErrUncertain, err)
	}
	next, err := json.Marshal(ownedContainer{Kind: "docker", Resource: reference})
	if err != nil {
		return err
	}
	result, err := p.owner.store.db.ExecContext(ctx, `UPDATE private_owners SET processes=json_set(processes,?,json(?)) WHERE id=? AND closing=0 AND json_extract(processes,?)=json(?)`, processPath(p.id), string(next), p.owner.ID, processPath(p.id), raw)
	return changed(result, err, "cannot bind canonical private container identity")
}
func (p *PrivateContainer) Finished(ctx context.Context) error {
	reference, raw, err := p.load(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} // Already acknowledged by this owner or recovery.
	if err != nil {
		return errors.Join(containerapi.ErrUncertain, err)
	}
	if reference.ContainerID == "" {
		return containerapi.ErrUncertain
	}
	client, err := containerapi.New(reference.Endpoint)
	if err != nil {
		return err
	}
	defer client.Close()
	observed, err := client.Observe(ctx, reference)
	if err != nil || observed.Present || observed.ContainerID != reference.ContainerID {
		return errors.Join(containerapi.ErrUncertain, err)
	}
	result, err := p.owner.store.db.ExecContext(ctx, `UPDATE private_owners SET processes=json_remove(processes,?) WHERE id=? AND closing=0 AND json_extract(processes,?)=json(?)`, processPath(p.id), p.owner.ID, processPath(p.id), raw)
	if err := changed(result, err, "cannot acknowledge private container removal"); err != nil {
		// A concurrent cleaner may already have acknowledged this exact removal.
		if _, _, currentErr := p.load(ctx); errors.Is(currentErr, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	return nil
}
