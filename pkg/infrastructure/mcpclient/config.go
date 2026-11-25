package mcpclient

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

type ServerConfig struct {
	Name                string   `mapstructure:"name"`
	URL                 string   `mapstructure:"url"`
	Scopes              []string `mapstructure:"scopes"`
	Tools               []string `mapstructure:"tools"`
	ClientID            string   `mapstructure:"client_id"`
	AuthorizationServer string   `mapstructure:"authorization_server"`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func Validate(configs []ServerConfig) error {
	names := make(map[string]bool)
	for _, config := range configs {
		if !namePattern.MatchString(config.Name) || len(config.Name) > 64 || names[config.Name] {
			return errors.New("MCP server names must be unique and contain 1–64 letters, digits, dots, underscores, or hyphens")
		}
		names[config.Name] = true
		if !validURL(config.URL) {
			return errors.New("MCP server URL must use " + urlRule)
		}
		if config.ClientID == "" && config.AuthorizationServer != "" {
			return fmt.Errorf("MCP server %s: authorization_server requires client_id", config.Name)
		}
		if config.ClientID != "" && !validURL(config.AuthorizationServer) {
			return fmt.Errorf("MCP server %s: client_id requires an authorization_server URL using %s", config.Name, urlRule)
		}
	}
	return nil
}

const urlRule = "HTTPS (HTTP is allowed on loopback), without credentials or fragments"

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && u.User == nil && u.Fragment == "" && (u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")))
}
