package controlplane

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoDB-backed remediation persistence (proposals, policy, audit). Proposals
// and policies are durable so a control-plane restart does not lose in-flight
// (approved-but-not-executed) proposals or an operator's policy/kill-switch.
// The domain structs carry bson tags, so there is no parallel doc type.

// --- proposals -----------------------------------------------------------

type mongoRemediationStore struct {
	coll *mongo.Collection
}

// NewMongoRemediationStore returns a durable RemediationStore and ensures the
// indexes the policy engine's queries rely on.
func NewMongoRemediationStore(ctx context.Context, db *mongo.Database) (RemediationStore, error) {
	coll := db.Collection("sre_remediations")
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := coll.Indexes().CreateMany(pctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenantId", Value: 1}, {Key: "status", Value: 1}}},
		{Keys: bson.D{{Key: "clusterId", Value: 1}, {Key: "status", Value: 1}}},
		{Keys: bson.D{{Key: "clusterId", Value: 1}, {Key: "target.kind", Value: 1},
			{Key: "target.namespace", Value: 1}, {Key: "target.name", Value: 1}}},
	}); err != nil {
		return nil, err
	}
	return &mongoRemediationStore{coll: coll}, nil
}

func (s *mongoRemediationStore) Create(ctx context.Context, p *RemediationProposal) error {
	_, err := s.coll.InsertOne(ctx, p)
	return err
}

func (s *mongoRemediationStore) Get(ctx context.Context, tenantID, id string) (*RemediationProposal, error) {
	var p RemediationProposal
	// Tenant scoping in the filter: no cross-tenant read.
	err := s.coll.FindOne(ctx, bson.M{"_id": id, "tenantId": tenantID}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *mongoRemediationStore) Update(ctx context.Context, p *RemediationProposal) error {
	res, err := s.coll.ReplaceOne(ctx, bson.M{"_id": p.ID, "tenantId": p.TenantID}, p)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mongoRemediationStore) List(ctx context.Context, tenantID, status string) ([]RemediationProposal, error) {
	filter := bson.M{"tenantId": tenantID}
	if status != "" {
		filter["status"] = status
	}
	cur, err := s.coll.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}))
	if err != nil {
		return nil, err
	}
	var out []RemediationProposal
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *mongoRemediationStore) ActiveCount(ctx context.Context, clusterID string) (int, error) {
	// Approved but not yet executed == in flight. A nil/absent execution matches
	// {execution: nil} in MongoDB.
	n, err := s.coll.CountDocuments(ctx, bson.M{
		"clusterId": clusterID, "status": string(StatusApproved), "execution": nil,
	})
	return int(n), err
}

func (s *mongoRemediationStore) LastExecutionForTarget(ctx context.Context, clusterID string, t TargetRef) (*time.Time, error) {
	var p RemediationProposal
	err := s.coll.FindOne(ctx,
		bson.M{
			"clusterId": clusterID, "target.kind": t.Kind,
			"target.namespace": t.Namespace, "target.name": t.Name,
			"execution": bson.M{"$ne": nil},
		},
		options.FindOne().SetSort(bson.D{{Key: "execution.finishedAt", Value: -1}}),
	).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fin := p.Execution.FinishedAt
	return &fin, nil
}

// --- policy --------------------------------------------------------------

type mongoPolicyStore struct {
	coll *mongo.Collection
}

// NewMongoPolicyStore returns a durable PolicyStore. The tenant id is the _id.
func NewMongoPolicyStore(_ context.Context, db *mongo.Database) PolicyStore {
	return &mongoPolicyStore{coll: db.Collection("sre_policies")}
}

func (s *mongoPolicyStore) Get(ctx context.Context, tenantID string) (Policy, error) {
	var p Policy
	err := s.coll.FindOne(ctx, bson.M{"_id": tenantID}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return DefaultPolicy(tenantID), nil // safe default until an operator sets one
	}
	if err != nil {
		return Policy{}, err
	}
	return p, nil
}

func (s *mongoPolicyStore) Put(ctx context.Context, p Policy) error {
	_, err := s.coll.ReplaceOne(ctx, bson.M{"_id": p.TenantID}, p, options.Replace().SetUpsert(true))
	return err
}

// --- audit ---------------------------------------------------------------

type mongoAuditStore struct {
	coll *mongo.Collection
}

// NewMongoAuditStore returns a durable, append-only AuditStore.
func NewMongoAuditStore(ctx context.Context, db *mongo.Database) (AuditStore, error) {
	coll := db.Collection("sre_audit")
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := coll.Indexes().CreateOne(pctx, mongo.IndexModel{
		Keys: bson.D{{Key: "tenantId", Value: 1}, {Key: "at", Value: -1}},
	}); err != nil {
		return nil, err
	}
	return &mongoAuditStore{coll: coll}, nil
}

func (s *mongoAuditStore) Append(ctx context.Context, ev AuditEvent) error {
	_, err := s.coll.InsertOne(ctx, ev)
	return err
}

func (s *mongoAuditStore) List(ctx context.Context, tenantID string) ([]AuditEvent, error) {
	cur, err := s.coll.Find(ctx, bson.M{"tenantId": tenantID},
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	var out []AuditEvent
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
