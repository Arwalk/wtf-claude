package api

import (
	"encoding/json"
	"strings"
)

// Profile is the subset of GET /api/oauth/profile this tool uses. Personal
// details (email, full name) are deliberately not parsed.
type Profile struct {
	PlanLabel          string // e.g. "Max plan"
	PlanName           string // e.g. "Max"
	RateLimitTier      string // e.g. "default_claude_max_20x"
	OrganizationType   string // e.g. "claude_max"
	SubscriptionStatus string // e.g. "active"
	ExtraUsageEnabled  bool
	HasClaudeMax       bool
	HasClaudePro       bool
}

// ParseProfile parses a profile response tolerantly.
func ParseProfile(b []byte) (*Profile, error) {
	top, ok := decodeObject(b)
	if !ok {
		return nil, ErrNotObject
	}
	var org struct {
		PlanLabel          flexString `json:"plan_display_label"`
		PlanName           flexString `json:"plan_display_name"`
		RateLimitTier      flexString `json:"rate_limit_tier"`
		OrganizationType   flexString `json:"organization_type"`
		SubscriptionStatus flexString `json:"subscription_status"`
		ExtraUsageEnabled  flexBool   `json:"has_extra_usage_enabled"`
	}
	var acct struct {
		HasMax flexBool `json:"has_claude_max"`
		HasPro flexBool `json:"has_claude_pro"`
	}
	if raw, ok := top["organization"]; ok && !isNull(raw) {
		_ = json.Unmarshal(raw, &org)
	}
	if raw, ok := top["account"]; ok && !isNull(raw) {
		_ = json.Unmarshal(raw, &acct)
	}
	return &Profile{
		PlanLabel:          strings.TrimSpace(string(org.PlanLabel)),
		PlanName:           strings.TrimSpace(string(org.PlanName)),
		RateLimitTier:      strings.TrimSpace(string(org.RateLimitTier)),
		OrganizationType:   strings.TrimSpace(string(org.OrganizationType)),
		SubscriptionStatus: strings.TrimSpace(string(org.SubscriptionStatus)),
		ExtraUsageEnabled:  bool(org.ExtraUsageEnabled),
		HasClaudeMax:       bool(acct.HasMax),
		HasClaudePro:       bool(acct.HasPro),
	}, nil
}
