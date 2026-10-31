package common

type ManagedWebBilling struct {
	Provider        string  `json:"provider"`
	Outcome         string  `json:"outcome,omitempty"`
	SearchRequests  int     `json:"search_requests"`
	FetchRequests   int     `json:"fetch_requests"`
	ModelRounds     int     `json:"model_rounds"`
	ModelQuota      int     `json:"model_quota"`
	Quota           int     `json:"quota"`
	CNYPerThousand  float64 `json:"cny_per_1k_calls"`
	USDToCNY        float64 `json:"usd_to_cny"`
	ApplyGroupRatio bool    `json:"apply_group_ratio"`
	SettlementError error   `json:"-"`
	ModelFailure    bool    `json:"-"`
}
