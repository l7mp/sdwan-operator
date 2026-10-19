package sdwan

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	User     string `yaml:"username"`
	Password string `yaml:"password"`
	URL      string `yaml:"url"`
	Insecure bool   `yaml:"insecure"`
	DryRun   bool   `yaml:"dry-run,omitempty"`
	// InPlaceUpdate edits the objects of the active central policy and pushes
	// the edit to the vSmarts, instead of deactivating the policy for every
	// change.
	InPlaceUpdate bool `yaml:"in-place-update,omitempty"`
}

// ReadConfig reads configuration from a yaml file
func ReadConfig(path string) (*Config, error) {
	// Read vManage config
	var conf Config
	f, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	err = yaml.Unmarshal(f, &conf)
	if err != nil {
		return nil, err
	}

	return &conf, nil
}
