package main

// Saved AI chat history: the last aiChatKeep question/answer pairs per user and
// cluster, in the ai_chats collection, so the AI tab survives reloads.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const aiChatKeep = 5

type chatTurn struct {
	Q  string    `bson:"q" json:"q"`
	A  string    `bson:"a" json:"a"`
	At time.Time `bson:"at" json:"at"`
}

func chatDocID(email, clusterID string) string {
	return strings.ToLower(email) + "|" + clusterID
}

// saveChatTurn appends one answered turn, keeping only the newest aiChatKeep.
// Best effort: a failed save never fails the chat.
func (a *app) saveChatTurn(email, clusterID, q, ans string) {
	if a.store == nil || a.store.chats == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = a.store.chats.UpdateOne(ctx,
		bson.M{"_id": chatDocID(email, clusterID)},
		bson.M{
			"$set": bson.M{"user": strings.ToLower(email), "cluster": clusterID, "updatedAt": time.Now().UTC()},
			"$push": bson.M{"turns": bson.M{
				"$each":  bson.A{chatTurn{Q: q, A: ans, At: time.Now().UTC()}},
				"$slice": -aiChatKeep,
			}},
		},
		options.Update().SetUpsert(true))
}

// handleAppChatHistory returns (GET) or clears (DELETE) the user's saved chat
// for one cluster: /api/app/clusters/{id}/chat.
func (a *app) handleAppChatHistory(w http.ResponseWriter, r *http.Request) {
	u, _, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	filter := bson.M{"_id": chatDocID(u.Email, r.PathValue("id"))}
	if r.Method == http.MethodDelete {
		if _, err := a.store.chats.DeleteOne(ctx, filter); err != nil {
			writeJSON(w, 500, map[string]any{"error": "could not clear chat"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	var doc struct {
		Turns []chatTurn `bson:"turns"`
	}
	err := a.store.chats.FindOne(ctx, filter).Decode(&doc)
	if err != nil && err != mongo.ErrNoDocuments {
		writeJSON(w, 500, map[string]any{"error": "could not load chat"})
		return
	}
	if doc.Turns == nil {
		doc.Turns = []chatTurn{}
	}
	writeJSON(w, 200, map[string]any{"turns": doc.Turns})
}
