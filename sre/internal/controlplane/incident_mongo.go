package controlplane

import (
	"context"
	"errors"
	"time"

	"clusterheart/sre/internal/contract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type mongoIncidentStore struct {
	coll *mongo.Collection
}

// NewMongoIncidentStore returns a MongoDB-backed IncidentStore and ensures
// indexes, including a PARTIAL unique index that forbids two *open* incidents
// sharing (clusterId, fingerprint).
func NewMongoIncidentStore(ctx context.Context, db *mongo.Database) (IncidentStore, error) {
	coll := db.Collection("sre_incidents")
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := coll.Indexes().CreateMany(pctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenantId", Value: 1}, {Key: "status", Value: 1}}},
		{
			Keys: bson.D{{Key: "clusterId", Value: 1}, {Key: "fingerprint", Value: 1}},
			Options: options.Index().SetUnique(true).
				SetPartialFilterExpression(bson.M{"status": "open"}),
		},
	}); err != nil {
		return nil, err
	}
	return &mongoIncidentStore{coll: coll}, nil
}

func (m *mongoIncidentStore) UpsertObservation(ctx context.Context, tenantID, clusterID string, at time.Time, obs observation) error {
	filter := bson.M{"clusterId": clusterID, "fingerprint": obs.fingerprint, "status": "open"}
	update := bson.M{
		"$setOnInsert": bson.M{
			"_id": newID("inc"), "tenantId": tenantID, "clusterId": clusterID,
			"fingerprint": obs.fingerprint, "type": obs.typ, "severity": obs.severity,
			"title": obs.title, "namespace": obs.namespace, "workload": obs.workload,
			"node": obs.node, "firstSeen": at, "status": "open",
		},
		"$set":  bson.M{"lastSeen": at},
		"$inc":  bson.M{"occurrences": 1},
		"$push": bson.M{"evidence": bson.M{"$each": bson.A{obs.evidence}, "$slice": -maxEvidence}},
	}
	_, err := m.coll.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	return err
}

func (m *mongoIncidentStore) ResolveMissing(ctx context.Context, clusterID string, active map[string]bool, at time.Time) (int, error) {
	fps := make([]string, 0, len(active))
	for fp := range active {
		fps = append(fps, fp)
	}
	res, err := m.coll.UpdateMany(ctx,
		bson.M{"clusterId": clusterID, "status": "open", "fingerprint": bson.M{"$nin": fps}},
		bson.M{"$set": bson.M{"status": "resolved", "resolvedAt": at}},
	)
	if err != nil {
		return 0, err
	}
	return int(res.ModifiedCount), nil
}

func (m *mongoIncidentStore) ListIncidents(ctx context.Context, tenantID, status string) ([]Incident, error) {
	filter := bson.M{"tenantId": tenantID}
	if status != "" {
		filter["status"] = status
	}
	cur, err := m.coll.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "lastSeen", Value: -1}}))
	if err != nil {
		return nil, err
	}
	var docs []incidentDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]Incident, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toIncident())
	}
	return out, nil
}

func (m *mongoIncidentStore) GetIncident(ctx context.Context, tenantID, id string) (Incident, error) {
	var d incidentDoc
	err := m.coll.FindOne(ctx, bson.M{"_id": id, "tenantId": tenantID}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Incident{}, ErrNotFound
	}
	if err != nil {
		return Incident{}, err
	}
	return d.toIncident(), nil
}

type incidentDoc struct {
	ID          string         `bson:"_id"`
	Fingerprint string         `bson:"fingerprint"`
	TenantID    string         `bson:"tenantId"`
	ClusterID   string         `bson:"clusterId"`
	Type        string         `bson:"type"`
	Severity    string         `bson:"severity"`
	Status      string         `bson:"status"`
	Title       string         `bson:"title"`
	Namespace   string         `bson:"namespace"`
	Workload    string         `bson:"workload"`
	Node        string         `bson:"node"`
	FirstSeen   time.Time      `bson:"firstSeen"`
	LastSeen    time.Time      `bson:"lastSeen"`
	Occurrences int            `bson:"occurrences"`
	Evidence    []EvidenceItem `bson:"evidence"`
	ResolvedAt  *time.Time     `bson:"resolvedAt,omitempty"`
}

func (d incidentDoc) toIncident() Incident {
	return Incident{
		ID: d.ID, Fingerprint: d.Fingerprint, TenantID: d.TenantID, ClusterID: d.ClusterID,
		Type: contract.ProblemType(d.Type), Severity: d.Severity, Status: d.Status, Title: d.Title,
		Namespace: d.Namespace, Workload: d.Workload, Node: d.Node,
		FirstSeen: d.FirstSeen, LastSeen: d.LastSeen, Occurrences: d.Occurrences,
		Evidence: d.Evidence, ResolvedAt: d.ResolvedAt,
	}
}
