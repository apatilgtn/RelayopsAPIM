package gateway

import (
	"github.com/relayops/apim/internal/graphql"
)

// persistedCache is the API's verified persisted-query cache. It lives on
// the gateway (not the snapshot) so config reloads keep it.
func (g *Gateway) persistedCache(apiID string) *graphql.PersistedCache {
	if c, ok := g.apq.Load(apiID); ok {
		return c.(*graphql.PersistedCache)
	}
	c, _ := g.apq.LoadOrStore(apiID, graphql.NewPersistedCache())
	return c.(*graphql.PersistedCache)
}
