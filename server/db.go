package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// store is the MongoDB-backed user store. One collection: users, keyed by a
// unique email index.
type store struct {
	client *mongo.Client
	users  *mongo.Collection
}

var errUserExists = errors.New("user already exists")

func openStore(ctx context.Context) (*store, error) {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		return nil, fmt.Errorf("MONGO_URI is not set (put it in server/.env)")
	}
	dbName := os.Getenv("MONGO_DB")
	if dbName == "" {
		dbName = "clusterheart"
	}

	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	client, err := mongo.Connect(pctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}
	if err := client.Ping(pctx, readpref.Primary()); err != nil {
		return nil, fmt.Errorf("mongo ping: %w", err)
	}

	s := &store{client: client, users: client.Database(dbName).Collection("users")}
	if _, err := s.users.Indexes().CreateOne(pctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "email", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("uk_email"),
	}); err != nil {
		return nil, fmt.Errorf("ensure email index: %w", err)
	}
	return s, nil
}

func (s *store) find(ctx context.Context, email string) (*User, error) {
	var u User
	err := s.users.FindOne(ctx, bson.M{"email": strings.ToLower(email)}).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *store) insert(ctx context.Context, u *User) error {
	_, err := s.users.InsertOne(ctx, u)
	if mongo.IsDuplicateKeyError(err) {
		return errUserExists
	}
	return err
}

func (s *store) setLastLogin(ctx context.Context, email string) error {
	_, err := s.users.UpdateOne(ctx, bson.M{"email": strings.ToLower(email)},
		bson.M{"$set": bson.M{"lastLogin": time.Now()}})
	return err
}

func (s *store) setApproved(ctx context.Context, email string, approved bool) error {
	_, err := s.users.UpdateOne(ctx, bson.M{"email": strings.ToLower(email)},
		bson.M{"$set": bson.M{"approved": approved}})
	return err
}

func (s *store) list(ctx context.Context) ([]User, error) {
	cur, err := s.users.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	var out []User
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// loadDotEnv reads simple KEY=VALUE lines from a .env file into the process
// environment (existing env vars win). Missing file is not an error.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}
