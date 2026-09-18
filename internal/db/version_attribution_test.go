package db

import (
	"strings"
	"testing"
)

func TestVersionQueriesIncludeNullableUploaderAttribution(t *testing.T) {
	for name, query := range map[string]string{
		"create": createVersionQuery,
		"list":   listVersionsQuery,
		"active": getActiveSiteVersionQuery,
	} {
		t.Run(name, func(t *testing.T) {
			for _, required := range []string{"uploaded_by", "uploader.username", "LEFT JOIN users AS uploader"} {
				if !strings.Contains(query, required) {
					t.Errorf("version query does not contain %q:\n%s", required, query)
				}
			}
		})
	}

	if !strings.Contains(createVersionQuery, "VALUES ($1, $2, $3, 'uploading', $4)") {
		t.Fatalf("create version does not persist its uploader argument:\n%s", createVersionQuery)
	}
	if !strings.Contains(listVersionsQuery, "ORDER BY v.version_number DESC") {
		t.Fatalf("version list ordering changed:\n%s", listVersionsQuery)
	}
	for _, required := range []string{"v.version_number = s.active_version", "v.status = 'active'"} {
		if !strings.Contains(getActiveSiteVersionQuery, required) {
			t.Fatalf("active version query lost %q:\n%s", required, getActiveSiteVersionQuery)
		}
	}
}
