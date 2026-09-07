package value

import "time"

type Filter struct {
	Database string
	Since    time.Time
	Until    time.Time
}

type PeriodHours struct {
	AllTime   float64 `json:"all_time"`
	ThisMonth float64 `json:"this_month"`
	ThisWeek  float64 `json:"this_week"`
}

type DatabaseHours struct {
	Name  string  `json:"name"`
	Hours float64 `json:"hours"`
}

type DayHours struct {
	Day   string  `json:"day"`
	Hours float64 `json:"hours"`
}

type Incident struct {
	Kind       string    `json:"kind"`
	Severity   string    `json:"severity"`
	EvidenceID string    `json:"evidence_id"`
	OccurredAt time.Time `json:"when"`
}

type IncidentSummary struct {
	Count         int        `json:"count"`
	CreditedHours float64    `json:"credited_hours"`
	Detail        []Incident `json:"detail"`
}

type Report struct {
	DBAHoursSaved         PeriodHours        `json:"dba_hours_saved"`
	ByFeature             map[string]float64 `json:"by_feature"`
	ByDatabase            []DatabaseHours    `json:"by_database"`
	IncidentsAvoided      IncidentSummary    `json:"incidents_avoided"`
	PotentialHoursPending float64            `json:"potential_hours_pending"`
	TrendDaily            []DayHours         `json:"trend_daily"`
}

type DatabaseMinutes struct {
	Name    string
	Minutes float64
}

type DayMinutes struct {
	Day     string
	Minutes float64
}

type Snapshot struct {
	AllTimeMinutes    float64
	MonthMinutes      float64
	WeekMinutes       float64
	ByFeatureMinutes  map[string]float64
	ByDatabaseMinutes []DatabaseMinutes
	PotentialMinutes  float64
	IncidentMinutes   float64
	Incidents         []Incident
	TrendMinutes      []DayMinutes
}

type CreditCandidate struct {
	ActionID          int64
	ActionType        string
	Outcome           string
	VerificationState string
	ModelMinutes      float64
	ModelVersion      int
}

type Credit struct {
	ActionID     int64
	Minutes      float64
	ModelVersion int
}

type IncidentCredit struct {
	DatabaseID      *int64
	Kind            string
	Severity        string
	CreditedMinutes float64
	EvidenceID      string
	ModelVersion    int
	DecisionID      int64
	ActionLogID     int64
	VerificationID  int64
	OccurredAt      time.Time
}

type IncidentRecord struct {
	ID              int64
	CreditedMinutes float64
}

type ZeroCreditResult struct {
	Applied         bool
	PreviousMinutes float64
}
