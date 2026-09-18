package db

import (
	"encoding/json"
	"time"
)

type User struct {
	ID        string
	Username  string
	IsAdmin   bool
	CreatedAt time.Time
	// Kind is users.kind: "person" or "team". A team is a namespace that owns
	// sites without being anybody and holds no credential of its own — people
	// act on it with their own keys or sessions.
	//
	// Only queries that select the column populate it; empty means "not
	// loaded", which reads as a person, the behaviour every caller had before
	// teams existed.
	Kind string
	// MemberCount is how many people are in a team, populated only by
	// ListAllUsers. Zero for a person, and zero wherever it was not loaded.
	MemberCount int
	// Email is the address the account signed up with or later claimed
	// (migration 0017). Display and migration data: it is read to find an
	// account at sign-in, and never grants access on its own. Empty means
	// either unknown or not loaded by this query.
	Email string
	// DisabledAt is users.disabled_at (migration 0023, design.md 6.4): set by
	// an admin offboarding a person. Nil means active, or not loaded by this
	// query — only ListAllUsers populates it today.
	DisabledAt *time.Time
}

// IsTeam reports whether this row is a team namespace rather than a person.
// A row whose Kind was never loaded is a person, which is what every account
// was before teams existed.
func (u User) IsTeam() bool { return u.Kind == "team" }

type Site struct {
	ID                 string
	UserID             string
	Name               string
	ActiveVersion      int
	Public             bool
	State              json.RawMessage
	UsesState          bool
	UsesVersionedState bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Version struct {
	ID               string
	SiteID           string
	VersionNumber    int
	S3Prefix         string
	Status           string
	UploadedBy       *string
	UploaderUsername *string
	CreatedAt        time.Time
}

type SiteAnalyticsSummary struct {
	SiteID         string
	TodayPageviews int64
	TodayVisits    int64
	Last7Pageviews int64
	Last7Visits    int64
	// Agent traffic over the same range, kept apart from readership rather than
	// discarded so any figure can still be explained.
	BotPageviews int64
	BotVisits    int64
}

type SiteAnalyticsDay struct {
	SiteID    string
	Day       time.Time
	Pageviews int64
	Visits    int64
}

type FileDownloadStat struct {
	Total int64
	Last7 int64
}
