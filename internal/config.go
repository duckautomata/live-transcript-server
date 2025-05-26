package internal

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Credentials struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"credentials"`
	Channels []string `yaml:"channels"`
}

func GetConfig() (Config, error) {
	configFile := "config.yaml"
	data, err := os.ReadFile(configFile)
	if err != nil {
		return Config{}, fmt.Errorf("unable to read yaml file: %v", err)
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return Config{}, fmt.Errorf("unable to unmarshal yaml: %v", err)
	}

	return config, nil
}
