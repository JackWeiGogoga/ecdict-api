package feedback

import (
	"strings"
	"unicode/utf8"

	"gogoga_dictionary/internal/apperr"
)

// Field limits in runes. Content keeps the historical 4000 limit.
const (
	MaxContentRunes          = 4000
	MaxCategoryRunes         = 64
	MaxUserIDRunes           = 128
	MaxDeviceRunes           = 128
	MaxIOSVersionRunes       = 32
	MaxAppVersionRunes       = 32
	MaxLocaleRunes           = 32
	MaxClientFeedbackIDRunes = 128
	MaxScreenshotURLRunes    = 512
)

// Normalize trims all fields and validates them. publicBaseURL is the Qiniu public origin; when set,
// screenshot_url must point into its /feedback/ folder, otherwise it must at least be https.
// The returned error is an *apperr.Validation with a client-safe message.
func (r *Record) Normalize(publicBaseURL string) error {
	fields := []struct {
		name string
		v    *string
		max  int
	}{
		{"client_feedback_id", &r.ClientFeedbackID, MaxClientFeedbackIDRunes},
		{"content", &r.Content, MaxContentRunes},
		{"category", &r.Category, MaxCategoryRunes},
		{"user_id", &r.UserID, MaxUserIDRunes},
		{"device", &r.Device, MaxDeviceRunes},
		{"ios_version", &r.IOSVersion, MaxIOSVersionRunes},
		{"app_version", &r.AppVersion, MaxAppVersionRunes},
		{"locale", &r.Locale, MaxLocaleRunes},
		{"screenshot_url", &r.ScreenshotURL, MaxScreenshotURLRunes},
	}
	for _, f := range fields {
		*f.v = strings.TrimSpace(*f.v)
		if utf8.RuneCountInString(*f.v) > f.max {
			if f.name == "content" {
				return apperr.Invalid("content too long")
			}
			return apperr.Invalid("%s too long", f.name)
		}
	}
	if r.ClientFeedbackID == "" {
		return apperr.Invalid("missing client_feedback_id")
	}
	if r.Content == "" {
		return apperr.Invalid("missing content")
	}
	if r.ScreenshotURL != "" {
		base := strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
		if base != "" {
			if !strings.HasPrefix(r.ScreenshotURL, base+"/feedback/") {
				return apperr.Invalid("invalid screenshot_url")
			}
		} else if !strings.HasPrefix(strings.ToLower(r.ScreenshotURL), "https://") {
			return apperr.Invalid("invalid screenshot_url")
		}
	}
	return nil
}
