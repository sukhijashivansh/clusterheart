package main

import (
	"encoding/json"
	"net/url"
	"sync"
)

// adminRow is one row of the admin Users table: the user plus how many
// Kubernetes clusters and servers they have connected.
type adminRow struct {
	User
	Known      bool // false when the control plane could not be asked
	Clusters   int
	ClustersOn int
	Servers    int
	ServersOn  int
}

// adminRows looks up every user's clusters and servers on the control plane,
// a few at a time. A failed lookup leaves that row's counts unknown.
func (a *app) adminRows(list []User) []adminRow {
	rows := make([]adminRow, len(list))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range list {
		rows[i].User = list[i]
		wg.Add(1)
		go func(r *adminRow) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			code, data, err := a.cpDo("GET", "/api/v1/clusters?tenantId="+url.QueryEscape(tenantFor(r.Email)), nil)
			if err != nil || code != 200 {
				return
			}
			var out struct {
				Clusters []struct {
					Kind      string `json:"kind"`
					Connected bool   `json:"connected"`
				} `json:"clusters"`
			}
			if json.Unmarshal(data, &out) != nil {
				return
			}
			r.Known = true
			for _, c := range out.Clusters {
				if c.Kind == "server" {
					r.Servers++
					if c.Connected {
						r.ServersOn++
					}
				} else {
					r.Clusters++
					if c.Connected {
						r.ClustersOn++
					}
				}
			}
		}(&rows[i])
	}
	wg.Wait()
	return rows
}
