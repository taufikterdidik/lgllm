package config

import (
	"strings"
	"testing"
)

func TestValidateMetrics(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "negative usage log size",
			yaml: "metrics:\n  usage_log_size: -1\n",
			want: "usage_log_size",
		},
		{
			name: "zero performance window",
			yaml: "metrics:\n  performance_window: 0\n",
			want: "performance_window",
		},
		{
			name: "pprof must be host:port",
			yaml: "metrics:\n  pprof_listen: nope\n",
			want: "host:port",
		},
		{
			name: "pprof must bind loopback",
			yaml: "metrics:\n  pprof_listen: 0.0.0.0:6060\n",
			want: "loopback",
		},
		{
			name: "valid loopback pprof",
			yaml: "metrics:\n  enabled: true\n  pprof_listen: 127.0.0.1:6060\n  usage_log_size: 100\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(shortcutBase + tc.yaml))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestMetricsDefaults(t *testing.T) {
	cfg, err := Parse([]byte(shortcutBase))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Metrics.Enabled {
		t.Error("metrics should be enabled by default")
	}
	if cfg.Metrics.UsageLogSize != 500 {
		t.Errorf("usage_log_size = %d", cfg.Metrics.UsageLogSize)
	}
	if cfg.Metrics.PerformanceWindow != 10 {
		t.Errorf("performance_window = %d, want 10", cfg.Metrics.PerformanceWindow)
	}
	if cfg.Metrics.PprofListen != "" {
		t.Errorf("pprof_listen = %q, want empty", cfg.Metrics.PprofListen)
	}
}
