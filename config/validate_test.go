package config

import (
	"testing"
)

func TestValidateEntropyConfig_InvalidThreshold(t *testing.T) {
	errs := validateEntropyConfig(BouncerConfig{
		EntropyEnabled:  true,
		EntropyThreshold: -1,
		EntropyMinToken: 20,
	})
	if len(errs) == 0 {
		t.Error("expected error for negative entropy threshold")
	}
}

func TestValidateEntropyConfig_Valid(t *testing.T) {
	errs := validateEntropyConfig(BouncerConfig{
		EntropyEnabled:  true,
		EntropyThreshold: 4.5,
		EntropyMinToken: 20,
	})
	if len(errs) > 0 {
		t.Errorf("expected no errors, got %v", errs)
	}
}

func TestValidateEntropyConfig_Disabled(t *testing.T) {
	errs := validateEntropyConfig(BouncerConfig{
		EntropyEnabled: false,
	})
	if len(errs) > 0 {
		t.Errorf("expected no errors when disabled, got %v", errs)
	}
}

func TestValidateTFIDFQuerySource_Invalid(t *testing.T) {
	errs := validateTFIDFQuerySource("invalid")
	if len(errs) == 0 {
		t.Error("expected error for invalid tfidf_query_source")
	}
}

func TestValidateTFIDFQuerySource_Valid(t *testing.T) {
	for _, v := range []string{"", "prior_messages", "self"} {
		errs := validateTFIDFQuerySource(v)
		if len(errs) > 0 {
			t.Errorf("expected no errors for %q, got %v", v, errs)
		}
	}
}
