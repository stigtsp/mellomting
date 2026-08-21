package accounting

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
)

// KeyUsage aggregates a key's request and token totals over the whole
// accounting file (PLAN §43-44).
type KeyUsage struct {
	KeyID        string
	Requests     int64
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	CachedTokens int64
}

// Report is the per-key usage over an accounting JSONL file.
type Report struct {
	Keys []KeyUsage
}

// ReportFile reads the JSONL accounting file at path and aggregates
// per-key usage (PLAN §44). A missing file yields an empty report;
// malformed lines are skipped so a torn write never corrupts the report.
func ReportFile(path string) (*Report, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Report{}, nil
		}
		return nil, err
	}
	defer f.Close()

	r := &Report{}
	var order []*KeyUsage
	byKey := make(map[string]*KeyUsage)
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var rec Record
			if jerr := json.Unmarshal(line, &rec); jerr == nil && rec.KeyID != "" {
				ku := byKey[rec.KeyID]
				if ku == nil {
					ku = &KeyUsage{KeyID: rec.KeyID}
					byKey[rec.KeyID] = ku
					order = append(order, ku)
				}
				ku.Requests++
				ku.InputTokens = satAdd(ku.InputTokens, rec.InputTokens)
				ku.OutputTokens = satAdd(ku.OutputTokens, rec.OutputTokens)
				ku.TotalTokens = satAdd(ku.TotalTokens, rec.TotalTokens)
				ku.CachedTokens = satAdd(ku.CachedTokens, rec.CachedTokens)
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	for _, ku := range order {
		r.Keys = append(r.Keys, *ku)
	}
	return r, nil
}
