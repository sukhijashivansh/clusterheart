package controlplane

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// mongoDiagnosisStore keeps the latest diagnosis per incident (upsert keyed on
// incidentId), tenant-scoped on read.
type mongoDiagnosisStore struct {
	coll *mongo.Collection
}

// NewMongoDiagnosisStore returns a durable DiagnosisStore.
func NewMongoDiagnosisStore(ctx context.Context, db *mongo.Database) (DiagnosisStore, error) {
	coll := db.Collection("sre_diagnoses")
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// One diagnosis per (tenant, incident): keep only the latest.
	if _, err := coll.Indexes().CreateOne(pctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "tenantId", Value: 1}, {Key: "incidentId", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return nil, err
	}
	return &mongoDiagnosisStore{coll: coll}, nil
}

func (s *mongoDiagnosisStore) Put(ctx context.Context, d Diagnosis) error {
	_, err := s.coll.ReplaceOne(ctx,
		bson.M{"tenantId": d.TenantID, "incidentId": d.IncidentID},
		d, options.Replace().SetUpsert(true))
	return err
}

func (s *mongoDiagnosisStore) GetByIncident(ctx context.Context, tenantID, incidentID string) (Diagnosis, error) {
	var d Diagnosis
	err := s.coll.FindOne(ctx, bson.M{"tenantId": tenantID, "incidentId": incidentID}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Diagnosis{}, ErrNotFound
	}
	if err != nil {
		return Diagnosis{}, err
	}
	return d, nil
}
