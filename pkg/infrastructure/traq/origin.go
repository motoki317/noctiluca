package traq

import (
	"errors"
	"net/url"
)

func ValidateOrigin(raw string) error {
	origin, err := url.Parse(raw)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Hostname() == "" ||
		origin.User != nil || (origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
		return errors.New("must be an http or https origin without credentials, a path, query, or fragment")
	}
	return nil
}
