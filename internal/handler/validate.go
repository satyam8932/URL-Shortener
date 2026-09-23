package handler

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"url_shortener/ent/schema"
	"url_shortener/internal/service"
)

const (
	maxURLLength   = 2048
	minAliasLength = 3
	maxExpiresIn   = 365 * 24 * time.Hour
)

// reservedAliases are paths the router already serves; an alias with one of
// these names could never be reached.
var reservedAliases = map[string]bool{
	"healthz": true,
	"readyz":  true,
	"shorten": true,
}

func (req shortenRequest) validate() (service.CreateInput, error) {
	if req.URL == "" {
		return service.CreateInput{}, errors.New("url is required")
	}
	if len(req.URL) > maxURLLength {
		return service.CreateInput{}, fmt.Errorf("url must be at most %d characters", maxURLLength)
	}
	u, err := url.ParseRequestURI(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return service.CreateInput{}, errors.New("url must be an absolute http or https URL")
	}

	if req.Alias != "" {
		if len(req.Alias) < minAliasLength || !isValidCode(req.Alias) {
			return service.CreateInput{}, fmt.Errorf(
				"alias must be %d-%d characters of letters, digits, '-' or '_'",
				minAliasLength, schema.ShortCodeMaxLen,
			)
		}
		if reservedAliases[req.Alias] {
			return service.CreateInput{}, errors.New("alias is reserved")
		}
	}

	var expiresIn time.Duration
	if req.ExpiresIn != nil {
		// Bounds are checked on the raw seconds: converting a huge value to a
		// Duration first would overflow and wrap into a small, valid-looking one.
		maxSeconds := int64(maxExpiresIn / time.Second)
		if *req.ExpiresIn <= 0 || *req.ExpiresIn > maxSeconds {
			return service.CreateInput{}, fmt.Errorf("expires_in must be between 1 and %d seconds", maxSeconds)
		}
		expiresIn = time.Duration(*req.ExpiresIn) * time.Second
	}

	return service.CreateInput{URL: req.URL, Alias: req.Alias, ExpiresIn: expiresIn}, nil
}

// isValidCode reports whether code could be a stored short code. Checking it
// before the lookup keeps junk paths such as /favicon.ico off the database.
func isValidCode(code string) bool {
	if code == "" || len(code) > schema.ShortCodeMaxLen {
		return false
	}
	for _, c := range code {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
