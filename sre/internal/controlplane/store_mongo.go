package controlplane

import (
	"context"
	"errors"
	"time"

	"clusterheart/sre/internal/contract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// snapshotRetention bounds how long raw snapshots live in the primary store. A
// TTL index prunes older documents - the DB is not a long-horizon telemetry
// sink (that is delegated to Prometheus/OTel later).
const snapshotRetention = 24 * time.Hour

type mongoStore struct {
	clusters  *mongo.Collection
	snapshots *mongo.Collection
}

type clusterDoc struct {
	ID                   string     `bson:"_id"`
	TenantID             string     `bson:"tenantId"`
	Name                 string     `bson:"name"`
	Environment          string     `bson:"environment"`
	Region               string     `bson:"region"`
	AgentTokenHash       string     `bson:"agentTokenHash"`
	RemediationTokenHash string     `bson:"remediationTokenHash,omitempty"`
	CreatedAt            time.Time  `bson:"createdAt"`
	LastHeartbeat        *time.Time `bson:"lastHeartbeat,omitempty"`
	AgentVersion         string     `bson:"agentVersion,omitempty"`
	K8sVersion           string     `bson:"k8sVersion,omitempty"`
	Kind                 string     `bson:"kind,omitempty"`
}

type snapshotDoc struct {
	ClusterID   string                   `bson:"clusterId"`
	CollectedAt time.Time                `bson:"collectedAt"`
	Snapshot    contract.ClusterSnapshot `bson:"snapshot"`
}

// ConnectMongo dials MongoDB, pings it, and returns the database handle so the
// control plane can share one client across the cluster store and the incident
// store. dbName defaults to "clusterheart" (shared with the rest of the project).
func ConnectMongo(ctx context.Context, uri, dbName string) (*mongo.Database, error) {
	if uri == "" {
		return nil, errors.New("MONGO_URI is not set")
	}
	if dbName == "" {
		dbName = "clusterheart"
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(pctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(pctx, readpref.Primary()); err != nil {
		return nil, err
	}
	return client.Database(dbName), nil
}

// NewMongoStore builds the cluster/telemetry store on an existing database and
// ensures its indexes.
func NewMongoStore(ctx context.Context, db *mongo.Database) (Store, error) {
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	s := &mongoStore{
		clusters:  db.Collection("sre_clusters"),
		snapshots: db.Collection("sre_snapshots"),
	}
	if _, err := s.clusters.Indexes().CreateOne(pctx, mongo.IndexModel{
		Keys: bson.D{{Key: "tenantId", Value: 1}},
	}); err != nil {
		return nil, err
	}
	if _, err := s.snapshots.Indexes().CreateMany(pctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "clusterId", Value: 1}, {Key: "collectedAt", Value: -1}}},
		{Keys: bson.D{{Key: "collectedAt", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(int32(snapshotRetention.Seconds()))},
	}); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *mongoStore) CreateCluster(ctx context.Context, c Cluster) error {
	_, err := s.clusters.InsertOne(ctx, clusterDoc{
		ID: c.ID, TenantID: c.TenantID, Name: c.Name, Environment: c.Environment,
		Region: c.Region, AgentTokenHash: c.AgentTokenHash, CreatedAt: c.CreatedAt, Kind: c.Kind,
	})
	if mongo.IsDuplicateKeyError(err) {
		return errors.New("cluster already exists")
	}
	return err
}

func (s *mongoStore) GetCluster(ctx context.Context, clusterID string) (Cluster, error) {
	var d clusterDoc
	err := s.clusters.FindOne(ctx, bson.M{"_id": clusterID}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Cluster{}, ErrNotFound
	}
	if err != nil {
		return Cluster{}, err
	}
	return docToCluster(d), nil
}

func (s *mongoStore) ListClusters(ctx context.Context, tenantID string) ([]Cluster, error) {
	cur, err := s.clusters.Find(ctx, bson.M{"tenantId": tenantID})
	if err != nil {
		return nil, err
	}
	var docs []clusterDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]Cluster, 0, len(docs))
	for _, d := range docs {
		out = append(out, docToCluster(d))
	}
	return out, nil
}

func (s *mongoStore) AllClusters(ctx context.Context) ([]Cluster, error) {
	cur, err := s.clusters.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	var docs []clusterDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]Cluster, 0, len(docs))
	for _, d := range docs {
		out = append(out, docToCluster(d))
	}
	return out, nil
}

func (s *mongoStore) RecordHeartbeat(ctx context.Context, clusterID string, hb contract.Heartbeat, at time.Time) error {
	set := bson.M{"lastHeartbeat": at, "agentVersion": hb.AgentVersion}
	if hb.K8sVersion != "" {
		set["k8sVersion"] = hb.K8sVersion
	}
	res, err := s.clusters.UpdateOne(ctx, bson.M{"_id": clusterID}, bson.M{"$set": set})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mongoStore) SaveSnapshots(ctx context.Context, clusterID string, snaps []contract.ClusterSnapshot) error {
	if _, err := s.GetCluster(ctx, clusterID); err != nil {
		return err
	}
	docs := make([]any, 0, len(snaps))
	for _, sn := range snaps {
		docs = append(docs, snapshotDoc{ClusterID: clusterID, CollectedAt: sn.CollectedAt, Snapshot: sn})
	}
	_, err := s.snapshots.InsertMany(ctx, docs)
	return err
}

func (s *mongoStore) LatestSnapshot(ctx context.Context, clusterID string) (*contract.ClusterSnapshot, error) {
	var d snapshotDoc
	err := s.snapshots.FindOne(ctx, bson.M{"clusterId": clusterID},
		options.FindOne().SetSort(bson.D{{Key: "collectedAt", Value: -1}})).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d.Snapshot, nil
}

func docToCluster(d clusterDoc) Cluster {
	return Cluster{
		ID: d.ID, TenantID: d.TenantID, Name: d.Name, Environment: d.Environment,
		Region: d.Region, AgentTokenHash: d.AgentTokenHash, RemediationTokenHash: d.RemediationTokenHash,
		CreatedAt: d.CreatedAt, LastHeartbeat: d.LastHeartbeat, AgentVersion: d.AgentVersion, K8sVersion: d.K8sVersion,
		Kind: d.Kind,
	}
}

func (s *mongoStore) SetRemediationTokenHash(ctx context.Context, clusterID, hash string) error {
	res, err := s.clusters.UpdateOne(ctx, bson.M{"_id": clusterID}, bson.M{"$set": bson.M{"remediationTokenHash": hash}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mongoStore) SetAgentTokenHash(ctx context.Context, clusterID, hash string) error {
	res, err := s.clusters.UpdateOne(ctx, bson.M{"_id": clusterID}, bson.M{"$set": bson.M{"agentTokenHash": hash}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mongoStore) RenameCluster(ctx context.Context, clusterID, name string) error {
	res, err := s.clusters.UpdateOne(ctx, bson.M{"_id": clusterID}, bson.M{"$set": bson.M{"name": name}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mongoStore) SetClusterEnvironment(ctx context.Context, clusterID, environment string) error {
	res, err := s.clusters.UpdateOne(ctx, bson.M{"_id": clusterID}, bson.M{"$set": bson.M{"environment": environment}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mongoStore) DeleteCluster(ctx context.Context, clusterID string) error {
	res, err := s.clusters.DeleteOne(ctx, bson.M{"_id": clusterID})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	_, _ = s.snapshots.DeleteMany(ctx, bson.M{"clusterId": clusterID})
	return nil
}
