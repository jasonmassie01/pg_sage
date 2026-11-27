package config

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIListenAddrFromFile reads only api.listen_addr from the YAML at path,
// expanding ${NAME} references as Load does. It serves one-shot tools such
// as the container healthcheck, which must answer without the database
// settings. An empty result means the file does not set the address.
func APIListenAddrFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc struct {
		API struct {
			ListenAddr string `yaml:"listen_addr"`
		} `yaml:"api"`
	}
	if err := yaml.Unmarshal([]byte(expandBracedEnv(string(data))), &doc); err != nil {
		return "", err
	}
	return strings.TrimSpace(doc.API.ListenAddr), nil
}
