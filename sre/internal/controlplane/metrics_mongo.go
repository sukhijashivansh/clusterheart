package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Collection names for the week-long server history. Both are MongoDB
// time-series collections: timeField "t", metaField "m" (the server, and for
// GPUs also the GPU), so MongoDB buckets each server's points together and
// expires them after metricRetention.
const (
	serverMetricsColl = "server_metrics"
	gpuMetricsColl    = "gpu_metrics"
)

// serverMetricDoc is one server sample.
type serverMetricDoc struct {
	T     time.Time       `bson:"t"`
	M     serverMetricKey `bson:"m"`
	CPU   float64         `bson:"cpu"`
	Mem   float64         `bson:"mem"`
	Swap  float64         `bson:"swap"`
	Disk  float64         `bson:"disk"`
	Load1 float64         `bson:"load1"`
	Load5 float64         `bson:"load5"`
	Rx    float64         `bson:"rx"`
	Tx    float64         `bson:"tx"`
}

type serverMetricKey struct {
	C string `bson:"c"` // server (cluster) id
}

// gpuMetricDoc is one GPU sample.
type gpuMetricDoc struct {
	T     time.Time    `bson:"t"`
	M     gpuMetricKey `bson:"m"`
	Util  float64      `bson:"util"`
	Mem   float64      `bson:"mem"`
	Temp  float64      `bson:"temp"`
	Power float64      `bson:"power"`
}

type gpuMetricKey struct {
	C    string `bson:"c"` // server (cluster) id
	I    int    `bson:"i"` // GPU index on that server
	UUID string `bson:"u,omitempty"`
	Name string `bson:"n,omitempty"`
}

// ensureMetricCollection creates a time-series collection, or on a MongoDB
// without time-series support a plain collection with a TTL index. It is safe
// to call on every start.
func ensureMetricCollection(ctx context.Context, db *mongo.Database, name string) error {
	err := db.CreateCollection(ctx, name, options.CreateCollection().
		SetTimeSeriesOptions(options.TimeSeries().SetTimeField("t").SetMetaField("m").SetGranularity("minutes")).
		SetExpireAfterSeconds(int64(metricRetention.Seconds())))
	var ce mongo.CommandError
	if err != nil && !(errors.As(err, &ce) && ce.Code == 48) { // 48 = NamespaceExists
		slog.Default().Warn("time-series collections unavailable; using a plain collection", "collection", name, "err", err)
	}
	coll := db.Collection(name)
	specs, err := db.ListCollectionSpecifications(ctx, bson.M{"name": name})
	if err != nil {
		return err
	}
	timeseries := len(specs) > 0 && specs[0].Type == "timeseries"
	models := []mongo.IndexModel{{Keys: bson.D{{Key: "m.c", Value: 1}, {Key: "t", Value: 1}}}}
	if !timeseries {
		models = append(models, mongo.IndexModel{Keys: bson.D{{Key: "t", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(int32(metricRetention.Seconds()))})
	}
	_, err = coll.Indexes().CreateMany(ctx, models)
	return err
}

func (s *mongoStore) AppendHostMetrics(ctx context.Context, clusterID string, hosts []HostPoint, gpus []GPUPoint) error {
	if len(hosts) > 0 {
		docs := make([]any, 0, len(hosts))
		for _, p := range hosts {
			docs = append(docs, serverMetricDoc{T: p.At, M: serverMetricKey{C: clusterID}, CPU: p.CPU, Mem: p.Mem, Swap: p.Swap,
				Disk: p.Disk, Load1: p.Load1, Load5: p.Load5, Rx: p.Rx, Tx: p.Tx})
		}
		if _, err := s.serverMetrics.InsertMany(ctx, docs); err != nil {
			return err
		}
	}
	if len(gpus) > 0 {
		docs := make([]any, 0, len(gpus))
		for _, g := range gpus {
			docs = append(docs, gpuMetricDoc{T: g.At, M: gpuMetricKey{C: clusterID, I: g.Index, UUID: g.UUID, Name: g.Name},
				Util: g.Util, Mem: g.Mem, Temp: g.Temp, Power: g.Power})
		}
		if _, err := s.gpuMetrics.InsertMany(ctx, docs); err != nil {
			return err
		}
	}
	return nil
}

// bucketExpr floors the sample time to the bucket size, in epoch milliseconds
// (works on MongoDB 4.0+, unlike $dateTrunc).
func bucketExpr(step time.Duration) bson.M {
	ms := step.Milliseconds()
	return bson.M{"$subtract": bson.A{bson.M{"$toLong": "$t"}, bson.M{"$mod": bson.A{bson.M{"$toLong": "$t"}, ms}}}}
}

func (s *mongoStore) HostHistory(ctx context.Context, clusterID string, from, to time.Time, step time.Duration) ([]HostBucket, []GPUHistory, error) {
	match := bson.M{"$match": bson.M{"m.c": clusterID, "t": bson.M{"$gte": from, "$lt": to}}}
	cur, err := s.serverMetrics.Aggregate(ctx, bson.A{
		match,
		bson.M{"$group": bson.M{
			"_id": bucketExpr(step),
			"cpu": bson.M{"$avg": "$cpu"}, "cpuMax": bson.M{"$max": "$cpu"},
			"mem": bson.M{"$avg": "$mem"}, "memMax": bson.M{"$max": "$mem"},
			"swap": bson.M{"$avg": "$swap"}, "disk": bson.M{"$avg": "$disk"},
			"load": bson.M{"$avg": "$load5"}, "loadMax": bson.M{"$max": "$load1"},
			"rx": bson.M{"$avg": "$rx"}, "rxMax": bson.M{"$max": "$rx"},
			"tx": bson.M{"$avg": "$tx"}, "txMax": bson.M{"$max": "$tx"},
		}},
		bson.M{"$sort": bson.M{"_id": 1}},
	})
	if err != nil {
		return nil, nil, err
	}
	var rows []struct {
		ID      int64   `bson:"_id"`
		CPU     float64 `bson:"cpu"`
		CPUMax  float64 `bson:"cpuMax"`
		Mem     float64 `bson:"mem"`
		MemMax  float64 `bson:"memMax"`
		Swap    float64 `bson:"swap"`
		Disk    float64 `bson:"disk"`
		Load    float64 `bson:"load"`
		LoadMax float64 `bson:"loadMax"`
		Rx      float64 `bson:"rx"`
		RxMax   float64 `bson:"rxMax"`
		Tx      float64 `bson:"tx"`
		TxMax   float64 `bson:"txMax"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, nil, err
	}
	hosts := make([]HostBucket, 0, len(rows))
	for _, r := range rows {
		hosts = append(hosts, HostBucket{T: time.UnixMilli(r.ID).UTC(), CPU: round2(r.CPU), CPUMax: r.CPUMax, Mem: round2(r.Mem), MemMax: r.MemMax,
			Swap: round2(r.Swap), Disk: round2(r.Disk), Load: round2(r.Load), LoadMax: r.LoadMax,
			Rx: round2(r.Rx), RxMax: r.RxMax, Tx: round2(r.Tx), TxMax: r.TxMax})
	}

	gcur, err := s.gpuMetrics.Aggregate(ctx, bson.A{
		match,
		bson.M{"$group": bson.M{
			"_id":  bson.M{"i": "$m.i", "t": bucketExpr(step)},
			"name": bson.M{"$last": "$m.n"},
			"util": bson.M{"$avg": "$util"}, "utilMax": bson.M{"$max": "$util"},
			"mem": bson.M{"$avg": "$mem"}, "memMax": bson.M{"$max": "$mem"},
			"temp": bson.M{"$avg": "$temp"}, "tempMax": bson.M{"$max": "$temp"},
			"power": bson.M{"$avg": "$power"},
		}},
		bson.M{"$sort": bson.D{{Key: "_id.i", Value: 1}, {Key: "_id.t", Value: 1}}},
	})
	if err != nil {
		return nil, nil, err
	}
	var grows []struct {
		ID struct {
			I int   `bson:"i"`
			T int64 `bson:"t"`
		} `bson:"_id"`
		Name    string  `bson:"name"`
		Util    float64 `bson:"util"`
		UtilMax float64 `bson:"utilMax"`
		Mem     float64 `bson:"mem"`
		MemMax  float64 `bson:"memMax"`
		Temp    float64 `bson:"temp"`
		TempMax float64 `bson:"tempMax"`
		Power   float64 `bson:"power"`
	}
	if err := gcur.All(ctx, &grows); err != nil {
		return nil, nil, err
	}
	var gpus []GPUHistory
	for _, r := range grows {
		if len(gpus) == 0 || gpus[len(gpus)-1].Index != r.ID.I {
			gpus = append(gpus, GPUHistory{Index: r.ID.I})
		}
		g := &gpus[len(gpus)-1]
		if r.Name != "" {
			g.Name = r.Name
		}
		g.Points = append(g.Points, GPUBucket{T: time.UnixMilli(r.ID.T).UTC(), Util: round2(r.Util), UtilMax: r.UtilMax,
			Mem: round2(r.Mem), MemMax: r.MemMax, Temp: round2(r.Temp), TempMax: r.TempMax, Power: round2(r.Power)})
	}
	return hosts, gpus, nil
}

// DeleteMetrics removes a deleted server's history. Older MongoDB versions
// cannot delete from time-series collections; the TTL removes it then.
func (s *mongoStore) DeleteMetrics(ctx context.Context, clusterID string) error {
	_, err1 := s.serverMetrics.DeleteMany(ctx, bson.M{"m.c": clusterID})
	_, err2 := s.gpuMetrics.DeleteMany(ctx, bson.M{"m.c": clusterID})
	return errors.Join(err1, err2)
}

// storeWithoutHistory hides the metric methods when the history collections
// could not be set up, so the control plane still starts and serves the rest.
type storeWithoutHistory struct{ Store }
