package config

import (
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.Port() != "8080" || c.SchemaPath != "" || c.LLM.MaxBodyBytes != 6<<20 ||
		c.LLM.MaxConcurrent != 32 || c.RateLimits.Upload != 5 || c.AnalyticsRetentionDays != 395 ||
		c.TTS.CacheMaxBytes != 2048<<20 || c.TTS.CacheDir != "data/tts-cache" {
		t.Fatalf("defaults = %+v", c)
	}
	if c.LLM.Enabled() || c.TTS.Enabled() || c.Feishu.Enabled() || c.Qiniu.Enabled() {
		t.Fatal("features should be disabled by default")
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"QINIU_ACCESS_KEY": "ak"}, "QINIU_* partially configured"},
		{map[string]string{"FEISHU_APP_ID": "x", "FEISHU_APP_SECRET": "y"}, "FEISHU_BITABLE_APP_TOKEN"},
		{map[string]string{"LLM_MAX_BODY_MB": "abc"}, "LLM_MAX_BODY_MB"},
		{map[string]string{"TRUSTED_PROXIES": "999.0.0.0/8"}, "TRUSTED_PROXIES"},
		{map[string]string{"HTTP_ADDR": "8080"}, "HTTP_ADDR"},
		{map[string]string{"LLM_ARK_API_KEY": "k", "LLM_ARK_MODELS": " , "}, "LLM_ARK_MODELS"},
	}
	for _, tc := range cases {
		_, err := load(envOf(tc.env))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v", tc.env, err)
		}
	}
}
