package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const (
	// ModelTypesPath is answered by the agent itself with the ids of its
	// declared decision models; it is never relayed (SYSTEMONE_PLAN §D13).
	// The edge reads types only here, so a server's own model list can
	// never type a model.
	ModelTypesPath = "/mellomting/model-types"
	// maxModelTypesBytes bounds the reply the edge accepts.
	maxModelTypesBytes = 64 << 10
)

// modelTypes is the reply on ModelTypesPath.
type modelTypes struct {
	Decision []string `json:"decision"`
}

// modelTypesHandler answers ModelTypesPath: the declared decision models
// of a decision server, a 404 for any other server.
func modelTypesHandler(d *Decision) http.Handler {
	if d == nil {
		return http.NotFoundHandler()
	}
	body, _ := json.Marshal(modelTypes{Decision: modelIDs(decisionModels(d))})
	return jsonHandler(body)
}

// decisionModelList answers the edge's GET /v1/models for a decision
// server, which has no list of its own, with the declared models.
func decisionModelList(d *Decision) http.Handler {
	type card struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	list := struct {
		Object string `json:"object"`
		Data   []card `json:"data"`
	}{Object: "list"}
	for _, m := range decisionModels(d) {
		list.Data = append(list.Data, card{ID: m.ID, Object: "model"})
	}
	body, _ := json.Marshal(list)
	return jsonHandler(body)
}

func jsonHandler(body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}

// readModelTypes asks an attachment which of its models are decision
// models. A 404 is an agent with none, or one older than the path. Any
// other failure, and a reply that is not exactly the documented object
// naming advertised ids once each, fails discovery: an agent that
// declares types and cannot be read must not attach untyped, which
// would put its decision models on the generation routes.
func readModelTypes(ctx context.Context, rt http.RoundTripper, advertised []advertised) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent.invalid"+ModelTypesPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, nil
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("GET %s: status %d", ModelTypesPath, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxModelTypesBytes+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: read failed", ModelTypesPath)
	}
	if len(data) > maxModelTypesBytes {
		return nil, fmt.Errorf("GET %s: reply exceeds %d bytes", ModelTypesPath, maxModelTypesBytes)
	}
	return parseModelTypes(data, advertised)
}

// parseModelTypes strictly decodes a model-types reply against the
// attachment's advertised ids.
func parseModelTypes(data []byte, advertised []advertised) (map[string]bool, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var mt struct {
		Decision *[]string `json:"decision"`
	}
	if err := dec.Decode(&mt); err != nil {
		return nil, errors.New("model types: invalid reply")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("model types: trailing data")
	}
	if mt.Decision == nil {
		return nil, errors.New("model types: decision list missing")
	}
	known := make(map[string]bool, len(advertised))
	for _, m := range advertised {
		known[m.ID] = true
	}
	out := make(map[string]bool, len(*mt.Decision))
	for _, id := range *mt.Decision {
		if !known[id] {
			return nil, fmt.Errorf("model types: %q is not an advertised model", id)
		}
		if out[id] {
			return nil, fmt.Errorf("model types: %q is listed twice", id)
		}
		out[id] = true
	}
	return out, nil
}
