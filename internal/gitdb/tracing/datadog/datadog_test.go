package datadog

import (
	"testing"
)

func TestEnvToStruct_ValidEnv(t *testing.T) {
	env := []string{"DD_APM_RECEIVER_ADDR=localhost:8126", "DD_APM_RECEIVER_SOCKET=/var/run/datadog/apm.socket"}
	var cfg config
	err := envToStruct(env, &cfg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.ApmAddress != "localhost:8126" {
		t.Errorf("expected ApmAddress to be 'localhost:8126', got %s", cfg.ApmAddress)
	}
	if cfg.ApmFile != "/var/run/datadog/apm.socket" {
		t.Errorf("expected ApmFile to be '/var/run/datadog/apm.socket', got %s", cfg.ApmFile)
	}
}

func TestEnvToStruct_EmptyEnv(t *testing.T) {
	var env []string
	var cfg config
	err := envToStruct(env, &cfg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.ApmAddress != "" {
		t.Errorf("expected ApmAddress to be empty, got %s", cfg.ApmAddress)
	}
	if cfg.ApmFile != "" {
		t.Errorf("expected ApmFile to be empty, got %s", cfg.ApmFile)
	}
}

func TestEnvToStruct_InvalidEnvFormat(t *testing.T) {
	env := []string{"INVALID_ENV"}
	var cfg config
	err := envToStruct(env, &cfg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.ApmAddress != "" {
		t.Errorf("expected ApmAddress to be empty, got %s", cfg.ApmAddress)
	}
	if cfg.ApmFile != "" {
		t.Errorf("expected ApmFile to be empty, got %s", cfg.ApmFile)
	}
}
