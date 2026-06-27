package koanf_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/infobloxopen/devedge-sdk/config"
	konfig "github.com/infobloxopen/devedge-sdk/config/koanf"
)

// TestYAMLFile_Basic verifies that KoanfSource loads YAML and integrates with config.Load.
func TestYAMLFile_Basic(t *testing.T) {
	dir := t.TempDir()
	yamlFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlFile, []byte("grpc_addr: \":7070\"\nlog_level: debug\n"), 0600); err != nil {
		t.Fatal(err)
	}

	src, err := konfig.YAMLFile(yamlFile)
	if err != nil {
		t.Fatalf("YAMLFile: %v", err)
	}

	// Use a custom struct to test raw Source.Get.
	v, ok := src.Get("grpc_addr")
	if !ok {
		t.Fatal("expected grpc_addr to be present")
	}
	if v != ":7070" {
		t.Errorf("grpc_addr: expected :7070 got %q", v)
	}

	// Also verify it integrates with config.Load through the Source seam.
	type opts struct {
		GRPCAddr string `config:"grpc_addr" default:":9090"`
		LogLevel string `config:"log_level" default:"info"`
	}
	var o opts
	if err := config.Load(&o, src); err != nil {
		t.Fatal(err)
	}
	if o.GRPCAddr != ":7070" {
		t.Errorf("GRPCAddr: expected :7070 got %q", o.GRPCAddr)
	}
	if o.LogLevel != "debug" {
		t.Errorf("LogLevel: expected debug got %q", o.LogLevel)
	}
}

// TestYAMLFile_CaseInsensitiveKey verifies that uppercase keys are found via lowercase lookup.
func TestYAMLFile_CaseInsensitiveKey(t *testing.T) {
	dir := t.TempDir()
	yamlFile := filepath.Join(dir, "config.yaml")
	// koanf lowercases keys; test that uppercase CONFIG keys match lowercase YAML keys.
	if err := os.WriteFile(yamlFile, []byte("grpc_addr: \":8181\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	src, err := konfig.YAMLFile(yamlFile)
	if err != nil {
		t.Fatal(err)
	}

	// Get with uppercase key (as used in config tags) should find the lowercase YAML key.
	v, ok := src.Get("GRPC_ADDR")
	if !ok {
		t.Fatal("expected GRPC_ADDR (uppercased) to find the yaml key grpc_addr")
	}
	if v != ":8181" {
		t.Errorf("expected :8181 got %q", v)
	}
}

// TestYAMLFile_MissingKey verifies Get returns ok=false for absent keys.
func TestYAMLFile_MissingKey(t *testing.T) {
	dir := t.TempDir()
	yamlFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlFile, []byte("grpc_addr: \":9090\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	src, err := konfig.YAMLFile(yamlFile)
	if err != nil {
		t.Fatal(err)
	}

	_, ok := src.Get("MISSING_KEY")
	if ok {
		t.Error("expected ok=false for missing key")
	}
}

// TestYAMLFile_ParseError verifies that a bad YAML file returns an error.
func TestYAMLFile_ParseError(t *testing.T) {
	dir := t.TempDir()
	yamlFile := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(yamlFile, []byte(":\tnotvalid:\n  - bad\n  yaml\n"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := konfig.YAMLFile(yamlFile)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}
