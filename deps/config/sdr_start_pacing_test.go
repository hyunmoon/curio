package config

import "testing"

func TestSDRStartPacingDefaultsDisabled(t *testing.T) {
	cfg := DefaultCurioConfig()
	if cfg.Subsystems.SealSDRMinStartInterval != 0 || cfg.Subsystems.SealSDRStartJitter {
		t.Fatal("SDR start pacing must remain disabled by default")
	}
}
