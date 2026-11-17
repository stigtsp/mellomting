package agent

import (
	"io"

	"mellomting/internal/config"
	"mellomting/internal/discovery"
)

const (
	// maxDiscoveryBytes bounds a model list.
	maxDiscoveryBytes = 1 << 20
	// probeMaxModels bounds the agent's own reading of its server's list;
	// the edge applies agents.max_models.
	probeMaxModels = 1024
)

// readModelList parses a model list with the same strict, fuzzed parser
// `mellomting init` uses (discovery.ParseModelCards): IDs sorted, the
// context window when the server reports one, nothing else.
func readModelList(r io.Reader, maxModels int) ([]discovery.Model, error) {
	return discovery.ParseModelCards(r, maxDiscoveryBytes, maxModels)
}

// validAgentModelID is config.ValidAgentModelID (PLAN §104.6).
func validAgentModelID(id string) bool { return config.ValidAgentModelID(id) }

func modelIDs(ms []discovery.Model) []string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return ids
}
