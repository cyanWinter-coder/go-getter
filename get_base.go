// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"context"
	"sync"
)

// getterClientMu guards the optional client pointer installed by Client.
// It is package-global rather than embedded in getter because some getters
// copy the zero-sized base value when constructing helper getters.
var getterClientMu sync.RWMutex

// getter is our base getter; it regroups
// fields all getters have in common.
type getter struct {
	client *Client
}

func (g *getter) SetClient(c *Client) {
	getterClientMu.Lock()
	defer getterClientMu.Unlock()
	g.client = c
}

func (g *getter) getClient() *Client {
	if g == nil {
		return nil
	}
	getterClientMu.RLock()
	defer getterClientMu.RUnlock()
	return g.client
}

// Context tries to returns the Contex from the getter's
// client. otherwise context.Background() is returned.
func (g *getter) Context() context.Context {
	if g == nil {
		return context.Background()
	}

	getterClientMu.RLock()
	c := g.client
	getterClientMu.RUnlock()

	if c == nil || c.Ctx == nil {
		return context.Background()
	}
	return c.Ctx
}
