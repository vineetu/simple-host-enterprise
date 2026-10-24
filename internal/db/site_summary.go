package db

import (
	"context"
	"encoding/json"
	"fmt"
)

type SiteSummary struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Headings    string `json:"headings"`
	Text        string `json:"text"`
}

func (s *SiteSummary) Scan(value any) error {
	*s = SiteSummary{}
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case []byte:
		return json.Unmarshal(v, s)
	case string:
		return json.Unmarshal([]byte(v), s)
	default:
		return fmt.Errorf("summary: unexpected database type %T", value)
	}
}

// SaveVersionSummary also serves the backfill: immutable versions need only
// one summary, and concurrent deploys have different version numbers.
func SaveVersionSummary(ctx context.Context, q Querier, siteID string, version int, summary SiteSummary) error {
	data, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `UPDATE versions SET summary = $3::jsonb
 WHERE site_id = $1 AND version_number = $2 AND summary IS NULL`, siteID, version, string(data))
	return err
}
