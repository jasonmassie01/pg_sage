package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type Profile string

const (
	ProfileStaffed    Profile = "staffed"
	ProfileUnattended Profile = "unattended"
)

type ChangeClass string

const (
	ChangeIndex            ChangeClass = "index"
	ChangeAnalyze          ChangeClass = "analyze"
	ChangeVacuum           ChangeClass = "vacuum"
	ChangeFreeze           ChangeClass = "freeze"
	ChangeAutovacuumTuning ChangeClass = "autovacuum_tuning"
	ChangeConfigGUC        ChangeClass = "config_guc"
	ChangeRetention        ChangeClass = "retention"
	ChangeFKIndex          ChangeClass = "fk_index"
	ChangeOnlineMigration  ChangeClass = "online_migration"
	ChangeBackendSignal    ChangeClass = "backend_signal"
	ChangeQueryHint        ChangeClass = "query_hint"
	ChangeSchemaChange     ChangeClass = "schema_change"
)

type BudgetLimit struct {
	Present bool
	value   *int64
}

func NewBudgetLimit(value int64) BudgetLimit {
	return BudgetLimit{Present: true, value: &value}
}

func NoCapBudget() BudgetLimit {
	return BudgetLimit{Present: true}
}

func (l BudgetLimit) NoCap() bool {
	return l.Present && l.value == nil
}

func (l BudgetLimit) Value() (int64, bool) {
	if l.value == nil {
		return 0, false
	}
	return *l.value, true
}

type Budgets struct {
	StorageBytes   BudgetLimit
	SpendDaily     BudgetLimit
	LLMTokensDaily BudgetLimit
}

// BlastRadius bounds what self-initiated changes may touch per window.
// MaxRowsRewritten is one budget shared by every kind; MaxTablesPerWindow
// is the performance budget's table limit (with
// RateLimits.MaxSelfInitiatedChangesPerWindow, its change limit), and
// Hygiene the housekeeping budget. See Document.Budget.
type BlastRadius struct {
	MaxRowsRewritten   int64
	MaxTablesPerWindow int64
	Hygiene            KindBudget
}

type RateLimits struct {
	MaxSelfInitiatedChangesPerWindow int64
}

type Document struct {
	Profile                 Profile
	AllowedChangeClasses    []ChangeClass
	ApprovalRequiredClasses []ChangeClass
	MaintenanceWindows      []string
	LockDurationCeilingMS   int64
	BlastRadius             BlastRadius
	Budgets                 Budgets
	RateLimits              RateLimits
	DeadlineOverrides       map[DeadlineKind]bool
	RefusalSet              []string
	UnknownClassification   string
	SerializeMode           string
}

type documentWire struct {
	AllowedChangeClasses    []ChangeClass   `json:"allowed_change_classes"`
	ApprovalRequiredClasses []ChangeClass   `json:"approval_required_classes,omitempty"`
	MaintenanceWindows      []string        `json:"maintenance_windows"`
	LockDurationCeilingMS   int64           `json:"lock_duration_ceiling_ms"`
	BlastRadius             blastRadiusWire `json:"blast_radius"`
	Budgets                 json.RawMessage `json:"budgets"`
	RateLimits              rateLimitsWire  `json:"rate_limits"`
	DeadlineOverrides       map[string]bool `json:"deadline_overrides"`
	RefusalSet              []string        `json:"refusal_set"`
	UnknownClassification   string          `json:"unknown_classification"`
	SerializeMode           string          `json:"serialize_mode"`
}

type budgetWire struct {
	StorageBytes   json.RawMessage `json:"storage_bytes"`
	SpendDaily     json.RawMessage `json:"spend_daily"`
	LLMTokensDaily json.RawMessage `json:"llm_tokens_daily"`
}

func ParseDocument(raw []byte) (Document, error) {
	var wire documentWire
	if err := decodeStrict(raw, &wire); err != nil {
		return Document{}, fmt.Errorf("policy document: %w", err)
	}
	budgets, err := parseBudgets(wire.Budgets)
	if err != nil {
		return Document{}, err
	}
	doc, err := documentFromWire(wire, budgets)
	if err != nil {
		return Document{}, err
	}
	if err := ValidateDocument(doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func MarshalDocument(doc Document) ([]byte, error) {
	if err := ValidateDocument(doc); err != nil {
		return nil, err
	}
	wire := map[string]any{
		"allowed_change_classes":    doc.AllowedChangeClasses,
		"approval_required_classes": doc.ApprovalRequiredClasses,
		"maintenance_windows":       doc.MaintenanceWindows,
		"lock_duration_ceiling_ms":  doc.LockDurationCeilingMS,
		"blast_radius":              blastRadiusJSON(doc),
		"budgets": map[string]any{
			"storage_bytes":    budgetJSONValue(doc.Budgets.StorageBytes),
			"spend_daily":      budgetJSONValue(doc.Budgets.SpendDaily),
			"llm_tokens_daily": budgetJSONValue(doc.Budgets.LLMTokensDaily),
		},
		"rate_limits":            rateLimitsJSON(doc),
		"deadline_overrides":     doc.DeadlineOverrides,
		"refusal_set":            doc.RefusalSet,
		"unknown_classification": doc.UnknownClassification,
		"serialize_mode":         doc.SerializeMode,
	}
	return json.Marshal(wire)
}

func budgetJSONValue(limit BudgetLimit) any {
	if value, ok := limit.Value(); ok {
		return value
	}
	return nil
}

func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func parseBudgets(raw []byte) (Budgets, error) {
	var wire budgetWire
	if len(raw) == 0 {
		return Budgets{}, fmt.Errorf("budgets: missing")
	}
	if err := decodeStrict(raw, &wire); err != nil {
		return Budgets{}, fmt.Errorf("budgets: %w", err)
	}
	storage, err := parseBudgetLimit("storage_bytes", wire.StorageBytes)
	if err != nil {
		return Budgets{}, err
	}
	spend, err := parseBudgetLimit("spend_daily", wire.SpendDaily)
	if err != nil {
		return Budgets{}, err
	}
	tokens, err := parseBudgetLimit("llm_tokens_daily", wire.LLMTokensDaily)
	if err != nil {
		return Budgets{}, err
	}
	return Budgets{storage, spend, tokens}, nil
}

func parseBudgetLimit(name string, raw []byte) (BudgetLimit, error) {
	if len(raw) == 0 {
		return BudgetLimit{}, fmt.Errorf("budgets.%s: missing", name)
	}
	if string(raw) == "null" {
		return NoCapBudget(), nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return BudgetLimit{}, fmt.Errorf("budgets.%s: %w", name, err)
	}
	return NewBudgetLimit(value), nil
}

func documentFromWire(wire documentWire, budgets Budgets) (Document, error) {
	radius, rates, err := blastRadiusFromWire(wire.BlastRadius, wire.RateLimits)
	if err != nil {
		return Document{}, err
	}
	overrides := make(map[DeadlineKind]bool, len(wire.DeadlineOverrides))
	for kind, enabled := range wire.DeadlineOverrides {
		overrides[DeadlineKind(kind)] = enabled
	}
	return Document{
		AllowedChangeClasses:    wire.AllowedChangeClasses,
		ApprovalRequiredClasses: wire.ApprovalRequiredClasses,
		MaintenanceWindows:      wire.MaintenanceWindows,
		LockDurationCeilingMS:   wire.LockDurationCeilingMS,
		BlastRadius:             radius,
		Budgets:                 budgets,
		RateLimits:              rates,
		DeadlineOverrides:       overrides,
		RefusalSet:              wire.RefusalSet,
		UnknownClassification:   wire.UnknownClassification,
		SerializeMode:           wire.SerializeMode,
	}, nil
}

func ValidateDocument(doc Document) error {
	if doc.UnknownClassification != "fail_closed" {
		return fmt.Errorf("unknown_classification must be fail_closed")
	}
	if doc.SerializeMode != "park" && doc.SerializeMode != "queue" {
		return fmt.Errorf("serialize_mode %q is invalid", doc.SerializeMode)
	}
	if doc.LockDurationCeilingMS < 0 {
		return fmt.Errorf("lock_duration_ceiling_ms cannot be negative")
	}
	if err := validateRefusalSet(doc.RefusalSet); err != nil {
		return err
	}
	if err := validateBlastRadius(doc); err != nil {
		return err
	}
	if err := validateBudgets(doc.Budgets); err != nil {
		return err
	}
	if err := validateClasses(doc.AllowedChangeClasses); err != nil {
		return err
	}
	if err := validateClasses(doc.ApprovalRequiredClasses); err != nil {
		return err
	}
	return validateWindows(doc.MaintenanceWindows)
}

func validateBudgets(budgets Budgets) error {
	limits := map[string]BudgetLimit{
		"storage_bytes":    budgets.StorageBytes,
		"spend_daily":      budgets.SpendDaily,
		"llm_tokens_daily": budgets.LLMTokensDaily,
	}
	for name, limit := range limits {
		if !limit.Present {
			return fmt.Errorf("budgets.%s: missing", name)
		}
		if value, ok := limit.Value(); ok && value < 0 {
			return fmt.Errorf("budgets.%s cannot be negative", name)
		}
	}
	return nil
}

func validateClasses(classes []ChangeClass) error {
	known := knownChangeClasses()
	for _, class := range classes {
		if !known[class] {
			return fmt.Errorf("unknown change class %q", class)
		}
	}
	return nil
}

func validateWindows(windows []string) error {
	if len(windows) == 0 {
		return fmt.Errorf("maintenance_windows cannot be empty")
	}
	for _, expression := range windows {
		if _, err := ParseWindow(expression); err != nil {
			return err
		}
	}
	return nil
}

func ValidateContract(contract ActionContract) error {
	if strings.TrimSpace(contract.ActionType) == "" {
		return fmt.Errorf("action type is missing")
	}
	if !knownRisk(contract.RiskTier) {
		return fmt.Errorf("unknown risk tier %q", contract.RiskTier)
	}
	for _, guardrail := range contract.Guardrails {
		if guardrail != GuardrailApprovalRequired {
			return fmt.Errorf("unknown guardrail %q", guardrail)
		}
	}
	if !knownRollbackClass(contract.RollbackClass) {
		return fmt.Errorf("unknown rollback class %q", contract.RollbackClass)
	}
	if !knownDropKind(contract.DropKind) {
		return fmt.Errorf("unknown drop kind %q", contract.DropKind)
	}
	return nil
}

func StaffedProfile() Document {
	doc := baseProfile()
	doc.Profile = ProfileStaffed
	doc.MaintenanceWindows = []string{"weekdays 01:00-05:00"}
	doc.DeadlineOverrides = map[DeadlineKind]bool{DeadlineXID: false, DeadlineDisk: false}
	return doc
}

func UnattendedProfile() Document {
	doc := baseProfile()
	doc.Profile = ProfileUnattended
	doc.MaintenanceWindows = []string{"always", "weekends"}
	doc.DeadlineOverrides = map[DeadlineKind]bool{DeadlineXID: true, DeadlineDisk: true}
	return doc
}

func baseProfile() Document {
	classes := allChangeClasses()
	approval := append([]ChangeClass{ChangeOnlineMigration}, AgentChangeClasses()...)
	return Document{
		AllowedChangeClasses:    append([]ChangeClass(nil), classes...),
		ApprovalRequiredClasses: approval,
		LockDurationCeilingMS:   3000,
		BlastRadius: BlastRadius{
			MaxRowsRewritten:   5000000,
			MaxTablesPerWindow: DefaultPerformanceTablesPerWindow,
			Hygiene:            DefaultHygieneBudget(),
		},
		Budgets:    Budgets{NoCapBudget(), NoCapBudget(), NewBudgetLimit(500000)},
		RateLimits: RateLimits{DefaultPerformanceChangesPerWindow},
		RefusalSet: []string{
			"rls_change", "grant_expansion", "major_upgrade",
			"non_dup_object_drop", "unrollbackable",
		},
		UnknownClassification: "fail_closed",
		SerializeMode:         "park",
	}
}

func allChangeClasses() []ChangeClass {
	return []ChangeClass{
		ChangeIndex, ChangeAnalyze, ChangeVacuum, ChangeFreeze,
		ChangeAutovacuumTuning, ChangeConfigGUC, ChangeRetention,
		ChangeFKIndex, ChangeOnlineMigration, ChangeBackendSignal, ChangeQueryHint,
		ChangeSchemaChange, ChangeAgentAccess, ChangeAgentDataWrite, ChangeAgentSchemaChange,
		ChangeAgentMaintenance, ChangeAgentSandbox, ChangeAgentEstate,
	}
}

func knownChangeClasses() map[ChangeClass]bool {
	known := make(map[ChangeClass]bool)
	for _, class := range allChangeClasses() {
		known[class] = true
	}
	return known
}

func knownRisk(risk RiskTier) bool {
	return risk == RiskReadOnly || risk == RiskSafe ||
		risk == RiskModerate || risk == RiskHigh
}
