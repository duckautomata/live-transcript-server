package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsReservedChannelNames(t *testing.T) {
	for name := range reservedChannelNames {
		cfg := Config{Channels: []ChannelConfig{{Name: name}}}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("channel %q: err = %v, want a reserved-name error", name, err)
		}
	}
	cfg := Config{Channels: []ChannelConfig{{Name: "doki"}, {Name: "mint"}}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("ordinary names: %v", err)
	}
	dup := Config{Channels: []ChannelConfig{{Name: "doki"}, {Name: "doki"}}}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate: err = %v", err)
	}
}
