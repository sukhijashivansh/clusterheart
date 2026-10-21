package controlplane

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type mongoRunbookStore struct {
	coll *mongo.Collection
}

// NewMongoRunbookStore returns a durable RunbookStore (one per tenant+type).
func NewMongoRunbookStore(ctx context.Context, db *mongo.Database) (RunbookStore, error) {
	coll := db.Collection("sre_runbooks")
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := coll.Indexes().CreateOne(pctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "tenantId", Value: 1}, {Key: "incidentType", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return nil, err
	}
	return &mongoRunbookStore{coll: coll}, nil
}

func (s *mongoRunbookStore) Upsert(ctx context.Context, rb Runbook) (Runbook, error) {
	filter := bson.M{"tenantId": rb.TenantID, "incidentType": rb.IncidentType}
	var prev Runbook
	err := s.coll.FindOne(ctx, filter).Decode(&prev)
	switch {
	case err == nil:
		rb.ID, rb.Version = prev.ID, prev.Version+1
	case errors.Is(err, mongo.ErrNoDocuments):
		rb.ID, rb.Version = newID("rb"), 1
	default:
		return Runbook{}, err
	}
	if _, err := s.coll.ReplaceOne(ctx, filter, rb, options.Replace().SetUpsert(true)); err != nil {
		return Runbook{}, err
	}
	return rb, nil
}

func (s *mongoRunbookStore) List(ctx context.Context, tenantID string) ([]Runbook, error) {
	cur, err := s.coll.Find(ctx, bson.M{"tenantId": tenantID})
	if err != nil {
		return nil, err
	}
	var out []Runbook
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *mongoRunbookStore) GetForType(ctx context.Context, tenantID, incidentType string) (Runbook, error) {
	var rb Runbook
	err := s.coll.FindOne(ctx, bson.M{"tenantId": tenantID, "incidentType": incidentType}).Decode(&rb)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Runbook{}, ErrNotFound
	}
	if err != nil {
		return Runbook{}, err
	}
	return rb, nil
}
