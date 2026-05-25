package config

type BillingModel string

const (
	BillingSubscription BillingModel = "subscription"
	BillingCredit       BillingModel = "credit"
	BillingFree         BillingModel = "free"
	BillingMixed        BillingModel = "mixed"
)

type QuotaSource string

const (
	QuotaSourceNone    QuotaSource = "none"
	QuotaSourceAPI     QuotaSource = "api"
	QuotaSourceHeaders QuotaSource = "headers"
)

type QuotaExtractionMode string

const (
	ExtractionModeSimpleJSON   QuotaExtractionMode = "simple_json"
	ExtractionModeMaxFromArray QuotaExtractionMode = "max_from_array"
	ExtractionModeHeaders      QuotaExtractionMode = "headers"
)

type QuotaExtractionConfig struct {
	Mode QuotaExtractionMode `json:"mode"`

	BalancePath string `json:"balance_path,omitempty"`

	ArrayPath     string `json:"array_path,omitempty"`
	ValueField    string `json:"value_field,omitempty"`
	ValueDivideBy int    `json:"value_divide_by,omitempty"`
	ResetField    string `json:"reset_field,omitempty"`
	ResetUnit     string `json:"reset_unit,omitempty"`
	LevelField    string `json:"level_field,omitempty"`

	RemainingHeader string `json:"remaining_header,omitempty"`
	LimitHeader     string `json:"limit_header,omitempty"`
	ResetHeader     string `json:"reset_header,omitempty"`
}

type BillingConfig struct {
	Model               BillingModel           `json:"model"`
	Period              string                 `json:"period,omitempty"`
	PeriodHours         int                    `json:"period_hours,omitempty"`
	IncludedUSD         float64                `json:"included_usd,omitempty"`
	BalanceUSD          float64                `json:"balance_usd,omitempty"`
	QuotaSource         QuotaSource            `json:"quota_source,omitempty"`
	QuotaURL            string                 `json:"quota_url,omitempty"`
	QuotaInterval       string                 `json:"quota_interval,omitempty"`
	QuotaTimeoutSeconds int                    `json:"quota_timeout_seconds,omitempty"`
	QuotaExtraction     *QuotaExtractionConfig `json:"quota_extraction,omitempty"`
	FreeOnly            bool                   `json:"free_only,omitempty"`
	FreeModels          []string               `json:"free_models,omitempty"`
}
